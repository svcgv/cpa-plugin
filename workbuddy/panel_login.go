// panel_login.go implements the panel-driven account login used by the
// WorkBuddy dashboard's "添加账号" dialog.
//
// The host's own auth card drives CN logins only (AuthProvider.StartLogin has no
// region input). International accounts are added from the panel, which can ask
// the user which service to sign in to. Each panel login owns:
//
//   - the region the user picked, resolved to a service and request profile;
//   - a session ID that the browser polls, so the upstream OAuth state and the
//     user's credentials never travel through the panel;
//   - an isolated cookie-jar HTTP client, because the CN flow binds login state
//     to cookies issued at auth/state.
//
// Tokens only ever travel backend → host auth store. The panel receives a
// status and an account summary.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// panelLoginTTL bounds one panel login flow. The reference implementation allows
// 10 minutes, which gives the user room to authenticate at the provider first.
const panelLoginTTL = 10 * time.Minute

// panelLoginSession is one in-flight panel login.
type panelLoginSession struct {
	ID        string
	Service   wbService
	Profile   oauthRequestProfile
	Client    *http.Client
	State     string // upstream OAuth state; never returned to the panel
	AuthURL   string
	ExpiresAt time.Time

	// mu serializes polls for this session; cancelMu guards only the cancelled
	// flag so cancelling never waits behind an in-flight network round-trip.
	mu        sync.Mutex
	cancelMu  sync.Mutex
	cancelled bool
	completed bool
	auth      *storedAuth
}

// markCancelled flips the cancellation flag without waiting for a poll.
func (s *panelLoginSession) markCancelled() {
	s.cancelMu.Lock()
	s.cancelled = true
	s.cancelMu.Unlock()
}

// isCancelled reports the current cancellation state.
func (s *panelLoginSession) isCancelled() bool {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	return s.cancelled
}

var (
	panelLogins       sync.Map // session ID -> *panelLoginSession
	panelLoginIDSeqMu sync.Mutex
)

// panelLoginPersist writes a completed login's credential. Overridable so tests
// can assert what would be stored without a host auth store.
var panelLoginPersist = persistServiceLoginAuth

// panelLoginTokenDomain, when non-empty, replaces the domain the token response
// reports. Tests use it to simulate an upstream routed to the wrong realm.
var panelLoginTokenDomain = ""

func init() {
	go func() {
		ticker := time.NewTicker(loginStatesPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			panelLogins.Range(func(key, value any) bool {
				if s, ok := value.(*panelLoginSession); ok && now.After(s.ExpiresAt.Add(loginTTL)) {
					panelLogins.Delete(key)
				}
				return true
			})
		}
	}()
}

// domainForService is the canonical credential domain for a service. It is
// written when a token response omits its own domain, so an account can never
// drift into the CN bucket by default.
func domainForService(s wbService) string {
	switch s {
	case serviceCodeBuddyGlobal:
		return "www.codebuddy.ai"
	case serviceWorkBuddyGlobal:
		return "www.workbuddy.ai"
	default:
		return "www.codebuddy.cn"
	}
}

// serviceFromRegion maps the panel's region selector onto a service.
//
// "国际版" means the WorkBuddy international service (www.workbuddy.ai): that is
// what this plugin has always called 国际版, what isGlobalDomain classifies as
// global, and what existing Global credentials carry as their domain. CodeBuddy
// international (www.codebuddy.ai) is a separate service and gets its own
// explicit selector value so the two are never conflated.
func serviceFromRegion(region string) (wbService, error) {
	switch region {
	case "", "cn":
		return serviceCN, nil
	case "workbuddy-global", "global":
		return serviceWorkBuddyGlobal, nil
	case "codebuddy-global", "codebuddy.ai":
		return serviceCodeBuddyGlobal, nil
	default:
		return "", fmt.Errorf("unsupported region %q (want cn, workbuddy-global or codebuddy-global)", region)
	}
}

// profileForServiceLogin resolves the request profile for a service login.
// CN keeps honouring the configured oauth_client_mode; each international
// service has a single client profile of its own.
func profileForServiceLogin(service wbService) oauthRequestProfile {
	if service == serviceCN {
		mode := oauthClientModeCLI
		if features := currentFeatureRuntime(); features != nil {
			mode = features.oauthClientMode
		}
		return oauthProfileForService(serviceCN, mode)
	}
	return oauthProfileForService(service, oauthClientModeCLI)
}

// nextPanelLoginID returns a unique, unguessable session ID.
func nextPanelLoginID() string {
	panelLoginIDSeqMu.Lock()
	defer panelLoginIDSeqMu.Unlock()
	return "wl-" + randomHex(16)
}

func panelLoginSessionFor(id string) (*panelLoginSession, error) {
	v, ok := panelLogins.Load(id)
	if !ok {
		return nil, fmt.Errorf("登录会话不存在或已失效，请重新发起")
	}
	return v.(*panelLoginSession), nil
}

// handlePanelLoginStart begins a login flow and returns the authorization URL.
// Body: {"region": "cn" | "global"}.
func handlePanelLoginStart(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Region string `json:"region"`
	}
	_ = json.Unmarshal(req.Body, &body)

	service, err := serviceFromRegion(body.Region)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	profile := profileForServiceLogin(service)

	client, err := newLoginClient()
	if err != nil {
		return map[string]any{"success": false, "error": "auth state failed: " + err.Error()}
	}
	stateReq, err := buildAuthStateRequest(profile)
	if err != nil {
		return map[string]any{"success": false, "error": "auth state failed: " + err.Error()}
	}
	data, _, err := doJSONRequest(client, stateReq)
	if err != nil {
		return map[string]any{"success": false, "error": "auth state failed: " + err.Error()}
	}
	var st authStateData
	if err := json.Unmarshal(data, &st); err != nil {
		return map[string]any{"success": false, "error": "auth state: parse failed"}
	}
	if st.State == "" {
		return map[string]any{"success": false, "error": "auth state: 上游未返回 state，请重试"}
	}
	authURL := st.AuthURL
	if authURL == "" {
		authURL = profile.origin + "/login?state=" + st.State
	}
	// The desktop clients append a version and a per-login session id; the
	// browser page uses them to finish the handshake.
	if authURL, err = decorateDesktopAuthURL(authURL, randomHex(16)); err != nil {
		return map[string]any{"success": false, "error": "auth state: invalid authUrl"}
	}

	session := &panelLoginSession{
		ID:        nextPanelLoginID(),
		Service:   service,
		Profile:   profile,
		Client:    client,
		State:     st.State,
		AuthURL:   authURL,
		ExpiresAt: time.Now().Add(panelLoginTTL),
	}
	panelLogins.Store(session.ID, session)
	return map[string]any{
		"success":    true,
		"session_id": session.ID,
		"url":        authURL,
		// CPA HTML-escapes every string in a plugin management response, so a
		// bare "&" in this URL reaches the panel as "&amp;" and the browser then
		// sees "amp;platform"/"amp;state" as query keys — the state is lost and
		// the login silently never completes. url_encoded is the percent-encoded
		// form, which contains no "&" to escape; the panel decodes it.
		"url_encoded": url.QueryEscape(authURL),
		"region":      service.region(),
		"service":     string(service),
		"expires_at":  session.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// handlePanelLoginStatus performs one poll of an in-flight login. Body:
// {"session_id": "..."}.
//
// Polling is idempotent: once a session has completed, every later poll returns
// the same account summary instead of re-running the token exchange.
func handlePanelLoginStatus(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(req.Body, &body)
	id := strings.TrimSpace(body.SessionID)
	if id == "" {
		return map[string]any{"status": "error", "error": "session_id is required"}
	}
	session, err := panelLoginSessionFor(id)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}

	if session.isCancelled() {
		return map[string]any{"status": "cancelled", "message": "登录已取消"}
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	// Re-check under the poll lock: a cancel may have landed while this poll
	// waited its turn.
	if session.isCancelled() {
		return map[string]any{"status": "cancelled", "message": "登录已取消"}
	}
	if session.completed {
		return panelLoginSuccessResponse(session)
	}
	if time.Now().After(session.ExpiresAt) {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": "登录已超时，请重新发起"}
	}

	pollClient, err := loginClientForCurrentRouting(&loginCtx{client: session.Client})
	if err != nil {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": "poll: " + err.Error()}
	}

	// auth/token is the authoritative login-status endpoint: it reports a
	// non-zero code while the user is still authenticating and code 0 with the
	// token bundle once finished. login/account sits behind the gateway and is
	// rejected until we hold a bearer, so probe the token first.
	tokenReq, err := buildAuthTokenRequest(session.Profile, session.State)
	if err != nil {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": "poll: token request failed"}
	}
	tokRaw, status, errTok := doJSONRequest(pollClient, tokenReq)
	if errTok != nil {
		// Transport failures and 5xx are real errors; 4xx/business codes mean
		// the user simply has not finished authorizing yet.
		if status == 0 || status >= 500 {
			panelLogins.Delete(id)
			return map[string]any{
				"status": "error",
				"error":  "上游暂时不可用，请稍后重试",
			}
		}
		return map[string]any{"status": "pending", "message": "等待浏览器完成授权"}
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return map[string]any{"status": "pending", "message": "等待浏览器完成授权"}
	}
	if panelLoginTokenDomain != "" {
		tok.Domain = panelLoginTokenDomain
	}

	accountReq, err := buildLoginAccountRequest(session.Profile, session.State, tok.AccessToken)
	if err != nil {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": "poll: account request failed"}
	}
	acctRaw, _, errAcct := doJSONRequest(pollClient, accountReq)
	if errAcct != nil {
		panelLogins.Delete(id)
		return map[string]any{
			"status": "error",
			"error":  "已取得令牌但读取账号信息失败，请重新登录：" + truncateRedacted(errAcct.Error(), 120),
		}
	}
	var acct accountData
	if err := json.Unmarshal(acctRaw, &acct); err != nil {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": "poll: account lookup parse failed"}
	}

	// The token exchange is the long step, so a cancel issued while it ran must
	// still prevent the credential from being written.
	if session.isCancelled() {
		return map[string]any{"status": "cancelled", "message": "登录已取消"}
	}
	sa, err := buildServiceLoginStoredAuth(session.Service, tok, acct)
	if err != nil {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": err.Error()}
	}
	if err := panelLoginPersist(sa, session.Service); err != nil {
		// Keep the session alive so the user can retry the save without having
		// to re-authorize in the browser.
		return map[string]any{"status": "error", "error": "保存凭证失败：" + err.Error()}
	}
	session.auth = sa
	session.completed = true
	return panelLoginSuccessResponse(session)
}

// panelLoginSuccessResponse renders the account summary for the panel. Access
// and refresh tokens are deliberately omitted.
func panelLoginSuccessResponse(session *panelLoginSession) map[string]any {
	sa := session.auth
	out := map[string]any{
		"status":  "success",
		"region":  session.Service.region(),
		"service": string(session.Service),
	}
	if sa != nil {
		out["uid"] = sa.Account.UID
		out["nickname"] = sa.Account.Nickname
		out["file"] = authFileNameForService(sa, session.Service)
	}
	return out
}

// handlePanelLoginCancel abandons one login session. Body: {"session_id": "..."}.
// The session is marked cancelled before removal so a late poll cannot write an
// auth file after the user has moved on.
func handlePanelLoginCancel(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(req.Body, &body)
	id := strings.TrimSpace(body.SessionID)
	if id == "" {
		return map[string]any{"success": false, "error": "session_id is required"}
	}
	v, ok := panelLogins.Load(id)
	if !ok {
		// Nothing to cancel: the session already expired or never existed.
		return map[string]any{"success": true, "cancelled": true}
	}
	if s, ok := v.(*panelLoginSession); ok {
		// Mark rather than delete so any poll already in flight observes the
		// cancellation and cannot persist a credential after the fact. The
		// janitor removes the entry once its TTL passes.
		s.markCancelled()
	}
	return map[string]any{"success": true, "cancelled": true}
}

// buildServiceLoginStoredAuth maps a token grant onto a stored credential and
// pins the credential domain to the service that was actually logged in to.
//
// The upstream token response carries a domain; it is authoritative for the
// service. A response that disagrees with the service the user selected means
// the request was routed to the wrong realm, and silently accepting it would
// store a credential the plugin would then route incorrectly.
func buildServiceLoginStoredAuth(service wbService, tok tokenData, acct accountData) (*storedAuth, error) {
	sa, err := buildLoginStoredAuth(tok, acct)
	if err != nil {
		return nil, err
	}
	if d := strings.TrimSpace(tok.Domain); d != "" && serviceFromDomain(d) != service {
		return nil, fmt.Errorf("登录返回的凭据域 %q 与所选区域不符，请重新登录", d)
	}
	sa.Auth.Domain = domainForService(service)
	return sa, nil
}
