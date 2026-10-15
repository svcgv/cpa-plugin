package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginFakeUpstream is a scriptable CodeBuddy login upstream. Each service gets
// its own host so a test can prove the plugin never crosses them.
type loginFakeUpstream struct {
	mu sync.Mutex
	// tokenReady controls whether auth/token reports the login as finished.
	tokenReady bool
	// hosts records which host answered which path.
	hosts []string
	paths []string
	// stateIssuedBy records the host that minted each OAuth state.
	stateIssuedBy map[string]string
}

func (f *loginFakeUpstream) client(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		host := req.URL.Host
		f.hosts = append(f.hosts, host)
		f.paths = append(f.paths, req.URL.Path)
		if f.stateIssuedBy == nil {
			f.stateIssuedBy = map[string]string{}
		}
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			state := "state-" + host
			f.stateIssuedBy[state] = host
			return testHTTPResponse(req, `{"code":0,"data":{"state":"`+state+`","authUrl":"https://`+host+`/login?state=`+state+`"}}`), nil
		case "/v2/plugin/auth/token":
			if !f.tokenReady {
				return testHTTPResponse(req, `{"code":1001,"msg":"login ing"}`), nil
			}
			domain := "www.codebuddy.cn"
			switch host {
			case "www.codebuddy.ai":
				domain = "www.codebuddy.ai"
			case "www.workbuddy.ai":
				domain = "www.workbuddy.ai"
			}
			return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"dt","refreshToken":"rt","expiresIn":3600,"domain":"`+domain+`"}}`), nil
		case "/v2/plugin/login/account":
			return testHTTPResponse(req, `{"code":0,"data":{"uid":"uid-1","nickname":"Nick"}}`), nil
		default:
			return testHTTPResponse(req, `{"code":404,"msg":"not found"}`), nil
		}
	})}
}

func withLoginUpstream(t *testing.T, f *loginFakeUpstream) {
	t.Helper()
	oldProxy := currentProxyState()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() { proxyState.Store(oldProxy) })
	oldClient := sharedHTTPClient()
	sharedClient = f.client(t)
	t.Cleanup(func() { sharedClient = oldClient })
}

func mgmtReq(t *testing.T, body any) pluginapi.ManagementRequest {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return pluginapi.ManagementRequest{Body: raw}
}

// capturedAuth is one credential the login flow tried to persist.
type capturedAuth struct {
	name string
	sa   *storedAuth
}

// authCollector captures persisted credentials. Logins for different regions run
// concurrently, so collection is mutex-protected.
type authCollector struct {
	mu  sync.Mutex
	got []capturedAuth
}

func (c *authCollector) add(name string, sa *storedAuth) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, capturedAuth{name: name, sa: sa})
}

func (c *authCollector) snapshot() []capturedAuth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedAuth(nil), c.got...)
}

// withCapturedPersist redirects credential persistence away from the host auth
// store so tests can assert what would have been written.
func withCapturedPersist(t *testing.T) *authCollector {
	t.Helper()
	c := &authCollector{}
	old := panelLoginPersist
	panelLoginPersist = func(sa *storedAuth, service wbService) error {
		c.add(authFileNameForService(sa, service), sa)
		return nil
	}
	t.Cleanup(func() { panelLoginPersist = old })
	return c
}

func TestPanelLoginStartSelectsServiceByRegion(t *testing.T) {
	cases := []struct {
		region      string
		wantHost    string
		wantService wbService
	}{
		{"", "copilot.tencent.com", serviceCN},
		{"cn", "copilot.tencent.com", serviceCN},
		{"global", "www.codebuddy.ai", serviceCodeBuddyGlobal},
	}
	for _, tc := range cases {
		t.Run(tc.region, func(t *testing.T) {
			f := &loginFakeUpstream{}
			withLoginUpstream(t, f)

			out := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": tc.region}))
			if out["success"] != true {
				t.Fatalf("start failed: %#v", out)
			}
			if got := string(serviceFor(&storedAuth{Auth: storedTokens{Domain: domainForServiceFromOut(out)}})); got != string(tc.wantService) {
				t.Fatalf("service = %q, want %q", got, tc.wantService)
			}
			if len(f.hosts) == 0 || f.hosts[0] != tc.wantHost {
				t.Fatalf("auth/state went to %v, want %q", f.hosts, tc.wantHost)
			}
			if !strings.Contains(out["url"].(string), tc.wantHost) {
				t.Fatalf("authorization URL = %q, want host %q", out["url"], tc.wantHost)
			}
		})
	}
}

func domainForServiceFromOut(out map[string]any) string {
	if s, ok := out["service"].(string); ok {
		return domainForService(wbService(s))
	}
	return ""
}

func TestPanelLoginRejectsUnknownRegion(t *testing.T) {
	out := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "mars"}))
	if out["success"] == true {
		t.Fatalf("unknown region accepted: %#v", out)
	}
	if !strings.Contains(strings.ToLower(out["error"].(string)), "region") {
		t.Fatalf("error = %v", out["error"])
	}
}

// A global login must persist a credential that resolves to the international
// service, and its file name must not collide with the CN file for the same UID.
func TestPanelLoginPersistsPerServiceCredential(t *testing.T) {
	for _, tc := range []struct {
		region       string
		wantDomain   string
		wantFilePref string
	}{
		{"cn", "www.codebuddy.cn", "workbuddy-uid-1.json"},
		{"global", "www.codebuddy.ai", "workbuddy-codebuddy-global-uid-1.json"},
	} {
		t.Run(tc.region, func(t *testing.T) {
			f := &loginFakeUpstream{tokenReady: true}
			withLoginUpstream(t, f)
			saved := withCapturedPersist(t)

			start := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": tc.region}))
			sessionID := start["session_id"].(string)

			status := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))
			if status["status"] != "success" {
				t.Fatalf("status = %#v", status)
			}
			if len(saved.snapshot()) != 1 {
				t.Fatalf("persisted %d credentials, want 1", len(saved.snapshot()))
			}
			got := saved.snapshot()[0]
			if got.sa.Auth.Domain != tc.wantDomain {
				t.Errorf("stored domain = %q, want %q", got.sa.Auth.Domain, tc.wantDomain)
			}
			if got.name != tc.wantFilePref {
				t.Errorf("file name = %q, want %q", got.name, tc.wantFilePref)
			}
			// The panel response must never expose token material.
			for _, key := range []string{"access_token", "refresh_token", "accessToken", "refreshToken", "state"} {
				if _, present := status[key]; present {
					t.Errorf("panel response exposed %q: %#v", key, status)
				}
			}
			if _, present := start["state"]; present {
				t.Errorf("start response exposed the upstream state: %#v", start)
			}
		})
	}
}

// A token whose domain disagrees with the selected region means the request hit
// the wrong realm. Storing it would route the account incorrectly.
func TestPanelLoginRejectsDomainServiceMismatch(t *testing.T) {
	f := &loginFakeUpstream{tokenReady: true}
	withLoginUpstream(t, f)
	saved := withCapturedPersist(t)

	start := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "cn"}))
	sessionID := start["session_id"].(string)

	// Force the token endpoint to answer with the international domain.
	orig := panelLoginTokenDomain
	defer func() { panelLoginTokenDomain = orig }()
	panelLoginTokenDomain = "www.codebuddy.ai"

	status := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))
	if status["status"] == "success" {
		t.Fatalf("mismatched domain accepted: %#v", status)
	}
	if len(saved.snapshot()) != 0 {
		t.Fatalf("mismatched credential was persisted: %#v", saved.snapshot())
	}
}

func TestPanelLoginPendingThenSuccess(t *testing.T) {
	f := &loginFakeUpstream{}
	withLoginUpstream(t, f)
	withCapturedPersist(t)

	start := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "cn"}))
	sessionID := start["session_id"].(string)

	if got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "pending" {
		t.Fatalf("status = %v, want pending", got)
	}

	f.mu.Lock()
	f.tokenReady = true
	f.mu.Unlock()

	if got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "success" {
		t.Fatalf("status = %v, want success", got)
	}
}

// Repeated status polls after completion must stay idempotent and must not
// persist a second credential.
func TestPanelLoginDuplicatePollIsIdempotent(t *testing.T) {
	f := &loginFakeUpstream{tokenReady: true}
	withLoginUpstream(t, f)
	saved := withCapturedPersist(t)

	start := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "cn"}))
	sessionID := start["session_id"].(string)

	for i := 0; i < 3; i++ {
		if got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "success" {
			t.Fatalf("poll %d status = %v", i, got)
		}
	}
	if len(saved.snapshot()) != 1 {
		t.Fatalf("persisted %d credentials across repeated polls, want 1", len(saved.snapshot()))
	}
}

// Cancelling must stop the flow: a later poll must not authorize or persist.
func TestPanelLoginCancelStopsLateAuthorization(t *testing.T) {
	f := &loginFakeUpstream{tokenReady: true}
	withLoginUpstream(t, f)
	saved := withCapturedPersist(t)

	start := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "cn"}))
	sessionID := start["session_id"].(string)

	if handlePanelLoginCancel(mgmtReq(t, map[string]any{"session_id": sessionID}))["cancelled"] != true {
		t.Fatalf("cancel did not report cancellation")
	}
	got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))["status"]
	if got != "cancelled" {
		t.Fatalf("status after cancel = %v, want cancelled", got)
	}
	if len(saved.snapshot()) != 0 {
		t.Fatalf("a cancelled flow persisted a credential: %#v", saved.snapshot())
	}
}

// Cancelling an unknown session is a no-op success so the panel can always
// clean up, but polling an unknown session is a hard error.
func TestPanelLoginUnknownSessionHandling(t *testing.T) {
	if handlePanelLoginCancel(mgmtReq(t, map[string]any{"session_id": "nope"}))["success"] != true {
		t.Fatalf("cancelling an unknown session should succeed")
	}
	status := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": "nope"}))
	if status["status"] != "error" {
		t.Fatalf("polling an unknown session: %#v", status)
	}
}

// Two concurrent logins for different regions must stay isolated: each poll
// persists only its own service's credential.
func TestPanelLoginConcurrentRegionsStayIsolated(t *testing.T) {
	f := &loginFakeUpstream{tokenReady: true}
	withLoginUpstream(t, f)
	saved := withCapturedPersist(t)

	cnStart := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "cn"}))
	glStart := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "global"}))
	cnID := cnStart["session_id"].(string)
	glID := glStart["session_id"].(string)
	if cnID == glID {
		t.Fatal("two logins shared a session id")
	}

	var wg sync.WaitGroup
	for _, id := range []string{cnID, glID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": id}))["status"]; got != "success" {
				t.Errorf("session %s status = %v", id, got)
			}
		}(id)
	}
	wg.Wait()

	if len(saved.snapshot()) != 2 {
		t.Fatalf("persisted %d credentials, want 2", len(saved.snapshot()))
	}
	domains := map[string]int{}
	for _, c := range saved.snapshot() {
		domains[c.sa.Auth.Domain]++
	}
	if domains["www.codebuddy.cn"] != 1 || domains["www.codebuddy.ai"] != 1 {
		t.Fatalf("persisted domains = %v", domains)
	}
}

// A save failure must keep the session alive so the user can retry without
// re-authorizing in the browser.
func TestPanelLoginSaveFailureAllowsRetry(t *testing.T) {
	f := &loginFakeUpstream{tokenReady: true}
	withLoginUpstream(t, f)

	attempts := 0
	old := panelLoginPersist
	panelLoginPersist = func(sa *storedAuth, service wbService) error {
		attempts++
		if attempts == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	defer func() { panelLoginPersist = old }()

	start := handlePanelLoginStart(mgmtReq(t, map[string]any{"region": "cn"}))
	sessionID := start["session_id"].(string)

	if got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "error" {
		t.Fatalf("first poll status = %v, want error", got)
	}
	if got := handlePanelLoginStatus(mgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "success" {
		t.Fatalf("retry status = %v, want success", got)
	}
	if attempts != 2 {
		t.Fatalf("persist attempts = %d, want 2", attempts)
	}
}

// The CN profile must keep honouring the configured desktop mode, while the
// international profiles use the browser User-Agent the gateways require.
func TestLoginProfilesPerService(t *testing.T) {
	cb := oauthProfileForService(serviceCodeBuddyGlobal, oauthClientModeCLI)
	if cb.userAgent != codeBuddyBrowserUA {
		t.Errorf("codebuddy-global UA = %q", cb.userAgent)
	}
	if !strings.HasPrefix(cb.stateURL, upstreamBaseCodeBuddyGlobal) {
		t.Errorf("codebuddy-global state URL = %q", cb.stateURL)
	}
	if !strings.Contains(cb.stateURL, "platform=ide") {
		t.Errorf("codebuddy-global state URL must use the ide platform: %q", cb.stateURL)
	}
	wb := oauthProfileForService(serviceWorkBuddyGlobal, oauthClientModeCLI)
	if !strings.HasPrefix(wb.stateURL, upstreamBaseGlobal) {
		t.Errorf("workbuddy-global state URL = %q", wb.stateURL)
	}
	if cb.refreshURL == wb.refreshURL {
		t.Error("the two international services must not share a refresh endpoint")
	}
	if cn := oauthProfileForService(serviceCN, oauthClientModeWorkBuddy); cn.userAgent == codeBuddyBrowserUA {
		t.Error("the CN desktop profile must keep its own User-Agent")
	}
}

// requestURLHost is used to assert which host a refresh would target.
func TestRefreshRequestTargetsServiceHost(t *testing.T) {
	for _, tc := range []struct {
		domain string
		host   string
	}{
		{"www.codebuddy.cn", "copilot.tencent.com"},
		{"www.codebuddy.ai", "www.codebuddy.ai"},
		{"www.workbuddy.ai", "www.workbuddy.ai"},
	} {
		sa := &storedAuth{
			Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: tc.domain},
			Account: storedAccount{UID: "u1"},
		}
		profile := oauthProfileForService(serviceFor(sa), oauthClientModeCLI)
		req, err := buildTokenRefreshRequest(profile, sa)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(req.URL.String())
		if err != nil {
			t.Fatal(err)
		}
		if u.Host != tc.host {
			t.Errorf("domain %s refresh host = %q, want %q", tc.domain, u.Host, tc.host)
		}
		if got := req.Header.Get("X-Refresh-Token"); got != "rt" {
			t.Errorf("X-Refresh-Token = %q", got)
		}
	}
}

// The keepalive/refresh path must target the credential's own service. Building
// it from the configured CN client mode would send an international refresh
// token to the CN gateway.
func TestRefreshCallUsesCredentialServiceNotConfigMode(t *testing.T) {
	// Configure the CN desktop profile; it must not leak into other services.
	oldFeatures := featureRuntime.Load()
	desktop, err := parseFeatureRuntime([]byte("oauth_client_mode: workbuddy\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(desktop)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })

	cases := []struct {
		name    string
		domain  string
		wantURL string
	}{
		{"cn", "www.codebuddy.cn", "https://copilot.tencent.com/v2/plugin/auth/token/refresh"},
		{"codebuddy global", "www.codebuddy.ai", "https://www.codebuddy.ai/v2/plugin/auth/token/refresh"},
		{"workbuddy global", "www.workbuddy.ai", "https://www.workbuddy.ai/v2/plugin/auth/token/refresh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotURL string
			oldProxy := currentProxyState()
			proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
			t.Cleanup(func() { proxyState.Store(oldProxy) })
			oldClient := sharedHTTPClient()
			sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gotURL = req.URL.String()
				return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`), nil
			})}
			t.Cleanup(func() { sharedClient = oldClient })

			sa := &storedAuth{
				Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: tc.domain},
				Account: storedAccount{UID: "u1"},
			}
			if _, _, _, err := refreshCall(sa); err != nil {
				t.Fatalf("refresh failed: %v", err)
			}
			if gotURL != tc.wantURL {
				t.Fatalf("refresh URL = %q, want %q", gotURL, tc.wantURL)
			}
		})
	}
}

// A refresh response carrying another service's domain must not re-route the
// account.
func TestRefreshDoesNotAdoptCrossServiceDomain(t *testing.T) {
	oldProxy := currentProxyState()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() { proxyState.Store(oldProxy) })
	oldClient := sharedHTTPClient()
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// A CodeBuddy-international account whose refresh answers with the CN domain.
		return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
	})}
	t.Cleanup(func() { sharedClient = oldClient })

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: "www.codebuddy.ai"},
		Account: storedAccount{UID: "u1"},
	}
	raw, err := handleRefreshAuth(mustJSON(pluginapi.AuthRefreshRequest{AuthID: "auth-1", StorageJSON: mustJSON(sa)}))
	if err != nil {
		t.Fatal(err)
	}
	// Extract the returned credential and confirm the service did not change.
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthRefreshResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	returned, err := parseStored(resp.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if serviceFor(returned) != serviceCodeBuddyGlobal {
		t.Fatalf("service after refresh = %q, want codebuddy-global", serviceFor(returned))
	}
}
