package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// qwFakeAuthUpstream serves the CN device-authorization poll endpoint.
type qwFakeAuthUpstream struct {
	mu sync.Mutex
	// ready controls whether the poll reports a completed grant.
	ready bool
	// uid is returned in a completed grant.
	uid string
	// polls counts how many times the poll endpoint was hit.
	polls int
}

func (f *qwFakeAuthUpstream) roundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Path != "/api/v1/deviceToken/poll" {
		return qwJSON(req, 404, `{"error":"not found"}`), nil
	}
	f.polls++
	if !f.ready {
		// 404 means "not authorized yet" for this endpoint.
		return qwJSON(req, 404, `{"error":"pending"}`), nil
	}
	uid := f.uid
	if uid == "" {
		uid = "uid-1"
	}
	return qwJSON(req, 200, `{"token":"dt-abc","refresh_token":"drt-xyz","user_id":"`+uid+`","expires_in":2592000000}`), nil
}

func qwJSON(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func withQWAuthUpstream(t *testing.T, f *qwFakeAuthUpstream) {
	t.Helper()
	oldClient := sharedHTTPClient()
	sharedClient = &http.Client{Transport: qwRoundTripFunc(f.roundTrip)}
	t.Cleanup(func() { sharedClient = oldClient })
	oldBase := devicePollBase
	devicePollBase = "https://openapi.test"
	t.Cleanup(func() { devicePollBase = oldBase })
}

type qwRoundTripFunc func(*http.Request) (*http.Response, error)

func (f qwRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func qwMgmtReq(t *testing.T, body any) pluginapi.ManagementRequest {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return pluginapi.ManagementRequest{Body: raw}
}

func withQWCapturedPersist(t *testing.T) *[]*storedAuth {
	t.Helper()
	var got []*storedAuth
	old := panelAuthPersist
	panelAuthPersist = func(sa *storedAuth) error {
		got = append(got, sa)
		return nil
	}
	t.Cleanup(func() { panelAuthPersist = old })
	return &got
}

func TestQoderPanelLoginHappyPath(t *testing.T) {
	f := &qwFakeAuthUpstream{}
	withQWAuthUpstream(t, f)
	saved := withQWCapturedPersist(t)

	start := handlePanelLoginStart(qwMgmtReq(t, nil))
	if start["success"] != true {
		t.Fatalf("start: %#v", start)
	}
	url := start["url"].(string)
	// CN only: the authorization URL must stay on the CN website.
	if !strings.HasPrefix(url, qoderWebsiteCN) {
		t.Fatalf("auth URL = %q, want prefix %q", url, qoderWebsiteCN)
	}
	for _, param := range []string{"challenge", "nonce", "machine_id", "client_id", "redirect_uri"} {
		if !strings.Contains(url, param+"=") {
			t.Errorf("auth URL missing %s: %q", param, url)
		}
	}
	if start["region"] != "cn" {
		t.Errorf("region = %v, want cn", start["region"])
	}
	sessionID := start["session_id"].(string)

	if got := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "pending" {
		t.Fatalf("status = %v, want pending", got)
	}
	if len(*saved) != 0 {
		t.Fatalf("pending poll persisted a credential")
	}

	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()

	status := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))
	if status["status"] != "success" {
		t.Fatalf("status = %#v", status)
	}
	if len(*saved) != 1 {
		t.Fatalf("persisted %d credentials, want 1", len(*saved))
	}
	sa := (*saved)[0]
	if sa.Auth.AccessToken != "dt-abc" || sa.Auth.RefreshToken != "drt-xyz" {
		t.Errorf("stored tokens = %#v", sa.Auth)
	}
	if sa.Auth.Domain != "qoder.com.cn" {
		t.Errorf("stored domain = %q", sa.Auth.Domain)
	}
	if sa.Auth.MachineID == "" {
		t.Errorf("stored machineId is empty: %#v", sa.Auth)
	}
	if !strings.Contains(url, "machine_id="+sa.Auth.MachineID) {
		t.Errorf("auth URL machine_id does not match persisted machineId: url=%q machineId=%q", url, sa.Auth.MachineID)
	}
	// The panel must never receive token material.
	for _, key := range []string{"access_token", "refresh_token", "token", "verifier", "nonce"} {
		if _, present := status[key]; present {
			t.Errorf("panel response exposed %q: %#v", key, status)
		}
	}
}

// Polling a completed session repeatedly must not create a second credential.
func TestQoderPanelLoginDuplicatePollIsIdempotent(t *testing.T) {
	f := &qwFakeAuthUpstream{ready: true}
	withQWAuthUpstream(t, f)
	saved := withQWCapturedPersist(t)

	start := handlePanelLoginStart(qwMgmtReq(t, nil))
	sessionID := start["session_id"].(string)
	for i := 0; i < 3; i++ {
		if got := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "success" {
			t.Fatalf("poll %d status = %v", i, got)
		}
	}
	if len(*saved) != 1 {
		t.Fatalf("persisted %d credentials across repeated polls, want 1", len(*saved))
	}
}

// Cancelling must stop the flow so a late grant cannot be persisted.
func TestQoderPanelLoginCancelStopsLateGrant(t *testing.T) {
	f := &qwFakeAuthUpstream{ready: true}
	withQWAuthUpstream(t, f)
	saved := withQWCapturedPersist(t)

	start := handlePanelLoginStart(qwMgmtReq(t, nil))
	sessionID := start["session_id"].(string)

	if handlePanelLoginCancel(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["cancelled"] != true {
		t.Fatal("cancel did not report cancellation")
	}
	if got := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "cancelled" {
		t.Fatalf("status after cancel = %v, want cancelled", got)
	}
	if len(*saved) != 0 {
		t.Fatalf("cancelled flow persisted a credential: %#v", *saved)
	}
}

func TestQoderPanelLoginUnknownSession(t *testing.T) {
	// Cancelling an unknown session succeeds so the panel can always clean up.
	if handlePanelLoginCancel(qwMgmtReq(t, map[string]any{"session_id": "nope"}))["success"] != true {
		t.Fatal("cancelling an unknown session should succeed")
	}
	status := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": "nope"}))
	if status["status"] != "error" {
		t.Fatalf("polling an unknown session: %#v", status)
	}
}

// A save failure must keep the session resumable without re-authorizing.
func TestQoderPanelLoginSaveFailureAllowsRetry(t *testing.T) {
	f := &qwFakeAuthUpstream{ready: true}
	withQWAuthUpstream(t, f)

	attempts := 0
	old := panelAuthPersist
	panelAuthPersist = func(sa *storedAuth) error {
		attempts++
		if attempts == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	defer func() { panelAuthPersist = old }()

	start := handlePanelLoginStart(qwMgmtReq(t, nil))
	sessionID := start["session_id"].(string)

	if got := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "error" {
		t.Fatalf("first poll status = %v, want error", got)
	}
	if got := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "success" {
		t.Fatalf("retry status = %v, want success", got)
	}
	if attempts != 2 {
		t.Fatalf("persist attempts = %d, want 2", attempts)
	}
}

// An OAuth re-login must preserve an existing PAT for the same UID: the PAT is
// the long-lived fallback the refresh routing depends on.
func TestQoderPanelLoginPreservesExistingPAT(t *testing.T) {
	f := &qwFakeAuthUpstream{ready: true, uid: "uid-pat"}
	withQWAuthUpstream(t, f)
	saved := withQWCapturedPersist(t)

	// The PAT lookup walks the host auth store; stub it via the keepalive seam.
	oldFiles := qoderHostAuthList
	qoderHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{AuthIndex: "idx-1", Name: "qoderwork-uid-pat.json", ID: "id-1"}}, nil
	}
	oldGet := qoderHostAuthGet
	qoderHostAuthGet = func(string) (*storedAuth, error) {
		return &storedAuth{
			Auth:    storedTokens{PersonalToken: "pt-keepme", Domain: "qoder.com.cn"},
			Account: storedAccount{UID: "uid-pat"},
		}, nil
	}
	t.Cleanup(func() {
		qoderHostAuthList = oldFiles
		qoderHostAuthGet = oldGet
	})

	start := handlePanelLoginStart(qwMgmtReq(t, nil))
	sessionID := start["session_id"].(string)
	if got := handlePanelLoginStatus(qwMgmtReq(t, map[string]any{"session_id": sessionID}))["status"]; got != "success" {
		t.Fatalf("status = %v", got)
	}
	if len(*saved) != 1 {
		t.Fatalf("persisted %d credentials", len(*saved))
	}
	if got := (*saved)[0].Auth.PersonalToken; got != "pt-keepme" {
		t.Fatalf("PersonalToken = %q, want the existing PAT preserved", got)
	}
}

// Start must be independent per session: two concurrent flows get distinct
// verifiers and session ids.
func TestQoderPanelLoginSessionsAreIndependent(t *testing.T) {
	f := &qwFakeAuthUpstream{}
	withQWAuthUpstream(t, f)

	a := handlePanelLoginStart(qwMgmtReq(t, nil))
	b := handlePanelLoginStart(qwMgmtReq(t, nil))
	if a["session_id"] == b["session_id"] {
		t.Fatal("two logins shared a session id")
	}
	if a["url"] == b["url"] {
		t.Fatal("two logins shared an authorization URL (PKCE challenge reused)")
	}
}
