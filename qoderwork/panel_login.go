// panel_login.go implements the panel-driven QoderWork login used by the
// dashboard's account dialog.
//
// The host's own auth card also starts this flow, but it has no place to show an
// authorization URL the user can copy, retry, or cancel. The panel dialog owns
// the whole interaction:
//
//   - the PKCE verifier and device nonce stay in this process; the panel only
//     receives a session id and the authorization URL;
//   - the session can be cancelled, which stops any further polling and
//     discards a late grant;
//   - CN only. The international Qoder realm uses different hosts, a different
//     inference protocol and different token endpoints, and is explicitly out of
//     scope for this plugin.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// panelLoginSession is one in-flight panel-driven device authorization.
type panelLoginSession struct {
	ID        string
	Region    qoderRegion
	Nonce     string
	Verifier  string
	MachineID string
	AuthURL   string
	ExpiresAt time.Time
	StartedAt int64

	// mu serializes polls for this session; cancelMu guards only the cancelled
	// flag so cancelling never waits behind an in-flight network round-trip.
	mu        sync.Mutex
	cancelMu  sync.Mutex
	cancelled bool
	completed bool
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

var panelLogins sync.Map // session ID -> *panelLoginSession

// Seams for tests: credential persistence, and the host auth lookups used to
// carry an existing PAT across an OAuth re-login.
var (
	panelAuthPersist  = persistPanelAuth
	qoderHostAuthList = hostAuthList
	qoderHostAuthGet  = hostAuthGet
)

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

func nextPanelLoginID() string {
	return "qw-panel-" + fmt.Sprintf("%d", time.Now().UnixNano())
}

// handlePanelLoginStart begins a device-authorization flow for the requested
// realm. Body: {"region": "cn" | "intl"}; an absent region keeps the historical
// CN behaviour.
func handlePanelLoginStart(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Region string `json:"region"`
	}
	_ = json.Unmarshal(req.Body, &body)
	region := parseRegionValue(body.Region)
	eps := endpointsForRegion(region)

	verifier, challenge := makePKCE()
	nonce := uuid.NewString()
	machineID := defaultMachineID()

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machineID)
	q.Set("client_id", eps.ClientID)
	q.Set("redirect_uri", eps.RedirectURI)
	authURL := eps.Website + "/device/selectAccounts?" + q.Encode()

	now := time.Now()
	session := &panelLoginSession{
		ID:        nextPanelLoginID(),
		Region:    region,
		Nonce:     nonce,
		Verifier:  verifier,
		MachineID: machineID,
		AuthURL:   authURL,
		ExpiresAt: now.Add(loginTTL),
		StartedAt: now.UnixNano(),
	}
	panelLogins.Store(session.ID, session)
	return map[string]any{
		"success":    true,
		"session_id": session.ID,
		"url":        authURL,
		// CPA HTML-escapes every string in a plugin management response, so the
		// "&" separators of this URL would reach the panel as "&amp;" and the
		// browser would parse "amp;nonce"/"amp;challenge" as query keys. The
		// percent-encoded form contains no "&" and survives intact.
		"url_encoded": url.QueryEscape(authURL),
		"region":      string(region),
		"service":     serviceNameForRegion(region),
		"expires_at":  session.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// handlePanelLoginStatus performs one poll. Body: {"session_id": "..."}.
// A completed session keeps returning its result so duplicate polls are safe.
func handlePanelLoginStatus(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(req.Body, &body)
	id := strings.TrimSpace(body.SessionID)
	if id == "" {
		return map[string]any{"status": "error", "error": "session_id is required"}
	}
	v, ok := panelLogins.Load(id)
	if !ok {
		return map[string]any{"status": "error", "error": "登录会话不存在或已失效，请重新发起"}
	}
	session := v.(*panelLoginSession)
	region := session.Region
	if region == "" {
		region = regionCN
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
		return map[string]any{"status": "success", "region": string(region), "service": serviceNameForRegion(region)}
	}
	if time.Now().After(session.ExpiresAt) {
		panelLogins.Delete(id)
		return map[string]any{"status": "error", "error": "登录已超时，请重新发起"}
	}

	tok, pending, err := pollDeviceTokenAt(endpointsForRegion(region).OpenAPI, session.Nonce, session.Verifier)
	if err != nil {
		return map[string]any{"status": "error", "error": "授权轮询失败：" + truncateRedacted(err.Error(), 140)}
	}
	if pending || tok == nil {
		return map[string]any{"status": "pending", "message": "等待浏览器完成授权"}
	}

	// The device poll is the long step, so a cancel issued while it ran must
	// still prevent the credential from being written.
	if session.isCancelled() {
		return map[string]any{"status": "cancelled", "message": "登录已取消"}
	}
	// Preserve any PAT already stored for this UID: an OAuth re-login must not
	// throw away the long-lived fallback used by the token-refresh routing.
	pat := ""
	if uid := strings.TrimSpace(tok.UserID); uid != "" {
		pat = existingPATForUIDVia(uid, qoderHostAuthList, qoderHostAuthGet)
	}
	sa := buildStoredAuthFromDeviceTokenWithPAT(tok, nil, pat)
	sa.Auth.MachineID = session.MachineID
	sa.Auth.Region = string(region)
	sa.Auth.Domain = domainForRegion(region)
	if err := panelAuthPersist(sa); err != nil {
		// Keep the session so the user can retry the save without authorizing
		// again in the browser.
		return map[string]any{"status": "error", "error": "保存凭证失败：" + err.Error()}
	}
	session.completed = true
	out := map[string]any{"status": "success", "region": string(region), "service": serviceNameForRegion(region)}
	out["uid"] = sa.Account.UID
	out["nickname"] = sa.Account.Nickname
	return out
}

// handlePanelLoginCancel abandons one session. Body: {"session_id": "..."}.
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
