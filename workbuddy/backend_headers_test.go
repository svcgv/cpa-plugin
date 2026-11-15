package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestBackendHeadersAddsClientIdentity(t *testing.T) {
	makeHeaders := func() http.Header {
		req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
		backendHeaders(req, &storedAuth{})
		return req.Header
	}

	first := makeHeaders()
	for name, want := range map[string]string{
		"X-IDE-Type":     "CLI",
		"X-IDE-Name":     "CLI",
		"X-IDE-Version":  "2.63.2",
		"X-Agent-Intent": "craft",
	} {
		if got := first.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	idPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	idHeaders := []string{
		"X-Request-ID",
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	}
	seen := make(map[string]string, len(idHeaders))
	for _, name := range idHeaders {
		value := first.Get(name)
		if !idPattern.MatchString(value) {
			t.Errorf("%s = %q, want 32 lowercase hex characters", name, value)
		}
		if previous, exists := seen[value]; exists {
			t.Errorf("%s duplicates %s with value %q", name, previous, value)
		}
		seen[value] = name
	}

	second := makeHeaders()
	for _, name := range idHeaders {
		if first.Get(name) == second.Get(name) {
			t.Errorf("%s was reused across requests: %q", name, first.Get(name))
		}
	}
}

func TestBackendHeadersMaasSource(t *testing.T) {
	// Personal / SaaS account: no enterprise id, so the hash header stays empty
	// and the model is a builtin one.
	personal := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	backendHeaders(personal, &storedAuth{})
	for name, want := range map[string]string{
		"X-Source-Channel-Id":    "workbuddy-enterprise",
		"X-Source-Model-Type":    "builtin",
		"X-Source-Client-Type":   "CodingCopilot",
		"X-Source-Enterprise-Id": "",
		"X-Inference-Caller":     "workbuddy-cpa",
	} {
		if got := personal.Header.Get(name); got != want {
			t.Errorf("personal %s = %q, want %q", name, got, want)
		}
	}

	// "personal" is the sentinel CodeBuddy uses for consumer accounts.
	sentinel := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	backendHeaders(sentinel, &storedAuth{Account: storedAccount{EnterpriseID: "personal"}})
	if got := sentinel.Header.Get("X-Source-Enterprise-Id"); got != "" {
		t.Errorf("personal sentinel hash = %q, want empty", got)
	}
	if got := sentinel.Header.Get("X-Source-Model-Type"); got != "builtin" {
		t.Errorf("personal sentinel model type = %q, want builtin", got)
	}

	// Enterprise account: HMAC-SHA256 keyed by the channel id, hex encoded.
	enterprise := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	backendHeaders(enterprise, &storedAuth{Account: storedAccount{EnterpriseID: "ent-42"}})
	if got := enterprise.Header.Get("X-Source-Model-Type"); got != "enterprise-custom" {
		t.Errorf("enterprise model type = %q, want enterprise-custom", got)
	}
	mac := hmac.New(sha256.New, []byte("workbuddy-enterprise"))
	mac.Write([]byte("ent-42"))
	if want, got := hex.EncodeToString(mac.Sum(nil)), enterprise.Header.Get("X-Source-Enterprise-Id"); got != want {
		t.Errorf("enterprise hash = %q, want %q", got, want)
	}

	// Nil auth must not panic (guarded executor paths pass a nil account).
	nilAuth := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	backendHeaders(nilAuth, &storedAuth{})
	if got := nilAuth.Header.Get("X-Source-Channel-Id"); got != "workbuddy-enterprise" {
		t.Errorf("channel id = %q, want workbuddy-enterprise", got)
	}
}
