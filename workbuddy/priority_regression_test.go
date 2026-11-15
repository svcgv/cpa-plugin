package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type priorityHostState struct {
	mu        sync.Mutex
	files     map[string][]byte
	names     map[string]string
	getErr    map[string]error
	saves     int
	lastSaved *pluginapi.HostAuthSaveRequest
	requests  map[string][][]byte
}

func newPriorityHostState() *priorityHostState {
	return &priorityHostState{
		files:    make(map[string][]byte),
		names:    make(map[string]string),
		getErr:   make(map[string]error),
		requests: make(map[string][][]byte),
	}
}

func (s *priorityHostState) install(t *testing.T) {
	t.Helper()
	old := setHostCallHook(s.call)
	lifecycleState.Range(func(key, _ any) bool { lifecycleState.Delete(key); return true })
	t.Cleanup(func() {
		setHostCallHook(old)
		lifecycleState.Range(func(key, _ any) bool { lifecycleState.Delete(key); return true })
	})
}

func (s *priorityHostState) call(method string, request []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[method] = append(s.requests[method], append([]byte(nil), request...))
	switch method {
	case pluginabi.MethodHostAuthList:
		entries := make([]pluginapi.HostAuthFileEntry, 0, len(s.files))
		for idx, name := range s.names {
			entries = append(entries, pluginapi.HostAuthFileEntry{AuthIndex: idx, ID: idx, Name: name})
		}
		return okEnvelope(rpcHostAuthListResponse{Files: entries})
	case pluginabi.MethodHostAuthGet:
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		if err := s.getErr[req.AuthIndex]; err != nil {
			return nil, err
		}
		raw, ok := s.files[req.AuthIndex]
		if !ok {
			return nil, fmt.Errorf("auth file %q not found", req.AuthIndex)
		}
		result, err := json.Marshal(map[string]any{
			"auth_index": req.AuthIndex,
			"name":       s.names[req.AuthIndex],
			"path":       "/auth/" + s.names[req.AuthIndex],
			"json":       json.RawMessage(raw),
		})
		if err != nil {
			return nil, err
		}
		return okEnvelope(json.RawMessage(result))
	case pluginabi.MethodHostAuthSave:
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		s.saves++
		s.lastSaved = &req
		for idx, name := range s.names {
			if strings.EqualFold(name, req.Name) {
				s.files[idx] = append([]byte(nil), req.JSON...)
				break
			}
		}
		result, _ := json.Marshal(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name})
		return okEnvelope(json.RawMessage(result))
	default:
		return nil, fmt.Errorf("unexpected host method %q", method)
	}
}

func (s *priorityHostState) document(t *testing.T, authIndex string) map[string]any {
	t.Helper()
	s.mu.Lock()
	raw := append([]byte(nil), s.files[authIndex]...)
	s.mu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("saved document invalid: %v", err)
	}
	return doc
}

// savedDocument inspects the last host.auth.save payload directly. This also
// catches canonical filename migrations that a fake store keyed by the old
// filename would otherwise miss.
func (s *priorityHostState) savedDocument(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	if s.lastSaved == nil {
		s.mu.Unlock()
		t.Fatal("host.auth.save was not called")
	}
	raw := append([]byte(nil), s.lastSaved.JSON...)
	s.mu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("saved document invalid: %v", err)
	}
	return doc
}

func (s *priorityHostState) savedName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSaved == nil {
		return ""
	}
	return s.lastSaved.Name
}

func (s *priorityHostState) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func priorityFixture(priority any, uid string, extra map[string]any) ([]byte, *storedAuth) {
	doc := map[string]any{
		"type":     providerName,
		"provider": providerName,
		"logo":     pluginLogoURL,
		"disabled": false,
		"note":     "existing note",
		"auth": map[string]any{
			"accessToken":  "fake-access",
			"refreshToken": "fake-refresh",
			"expiresAt":    1999999999,
			"domain":       domainForService(serviceCN),
		},
		"account": map[string]any{"uid": uid, "nickname": "test account"},
		"custom":  map[string]any{"keep": true},
	}
	if priority != nil {
		doc["priority"] = priority
	}
	for k, v := range extra {
		doc[k] = v
	}
	raw, _ := json.Marshal(doc)
	sa, err := parseStored(raw)
	if err != nil {
		panic(err)
	}
	return raw, sa
}

func assertPriorityDocument(t *testing.T, doc map[string]any, want any) {
	t.Helper()
	got, ok := doc["priority"]
	if want == nil {
		if ok {
			t.Fatalf("priority unexpectedly present: %#v", got)
		}
	} else if !ok {
		t.Fatalf("priority missing: %#v", doc)
	} else if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("priority = %#v, want %#v", got, want)
	}
	if value, ok := doc["custom"].(map[string]any); !ok || value["keep"] != true {
		t.Fatalf("unknown metadata lost: %#v", doc["custom"])
	}
}

func expectedToken(action string) string {
	switch action {
	case "refresh":
		return "fake-access-refreshed"
	case "relogin", "import-pat":
		return "fake-access-relogin"
	default:
		return "fake-access"
	}
}

func assertLifecycleFields(t *testing.T, action string, doc map[string]any) {
	t.Helper()
	switch action {
	case "disable", "session-dead":
		if doc["disabled"] != true {
			t.Fatalf("%s: disabled = %#v", action, doc["disabled"])
		}
	case "reenable":
		if doc["disabled"] != false {
			t.Fatalf("%s: disabled = %#v", action, doc["disabled"])
		}
	}
}

func TestPriorityPersistenceMatrix(t *testing.T) {
	priorities := []any{25000, 0, -7, "-15", nil}
	for _, priority := range priorities {
		label := "unset"
		if priority != nil {
			label = fmt.Sprint(priority)
		}
		t.Run("priority="+label, func(t *testing.T) {
			for _, action := range []string{"sync-note", "disable", "reenable", "refresh", "relogin", "session-dead"} {
				t.Run(action, func(t *testing.T) {
					host := newPriorityHostState()
					raw, sa := priorityFixture(priority, "priority-uid", nil)
					host.files["idx-a"] = raw
					host.names["idx-a"] = "workbuddy.json"
					host.install(t)

					var err error
					switch action {
					case "sync-note":
						err = syncAuthNote("idx-a", "id-a", sa, nil, false)
					case "disable":
						err = disableAuth("idx-a", "id-a", sa, nil, "quota")
					case "reenable":
						err = reenableAuth("idx-a", "id-a", sa, &creditsSummary{TotalRemain: 100, Packages: []packageSummary{{Name: "plan"}}})
					case "refresh":
						sa.Auth.AccessToken = "fake-access-refreshed"
						err = persistAuthTokens("idx-a", sa)
					case "relogin":
						sa.Auth.AccessToken = "fake-access-relogin"
						err = persistServiceLoginAuth(sa, serviceCN)
					case "session-dead":
						err = markSessionDead("idx-a", "id-a", sa)
					}
					if err != nil {
						t.Fatalf("%s: %v", action, err)
					}
					doc := host.savedDocument(t)
					assertPriorityDocument(t, doc, priority)
					wantName := "workbuddy.json"
					switch action {
					case "sync-note", "disable", "reenable":
						wantName = "workbuddy-priority-uid.json"
					}
					if savedName := host.savedName(); savedName != wantName {
						t.Fatalf("%s: saved name = %q, want %q", action, savedName, wantName)
					}
					if _, stillLegacy := host.files["idx-a"]; !stillLegacy {
						t.Fatalf("%s: legacy physical record disappeared", action)
					}
					if host.saveCount() != 1 {
						t.Fatalf("%s: saves = %d, want 1", action, host.saveCount())
					}
					stored, err := parseStored(mustJSON(doc))
					if err != nil {
						t.Fatalf("%s: saved credential cannot be parsed: %v", action, err)
					}
					if stored.Account.UID != "priority-uid" {
						t.Fatalf("%s: UID = %q", action, stored.Account.UID)
					}
					if stored.Auth.AccessToken != expectedToken(action) {
						t.Fatalf("%s: access token = %q, want %q", action, stored.Auth.AccessToken, expectedToken(action))
					}
					assertLifecycleFields(t, action, doc)
				})
			}
		})
	}
}

func TestImportAuthPreservesPriority(t *testing.T) {
	for _, priority := range []any{25000, 0, -7, "-15", nil} {
		host := newPriorityHostState()
		raw, _ := priorityFixture(priority, "import-uid", nil)
		host.files["idx-import"] = raw
		host.names["idx-import"] = providerName + "-import-uid.json"
		host.install(t)
		result := handleImportAuth(pluginapi.ManagementRequest{Body: mustJSON(map[string]any{"json": json.RawMessage(raw)})})
		if result["success"] != true {
			t.Fatalf("import failed for priority %#v: %#v", priority, result)
		}
		assertPriorityDocument(t, host.savedDocument(t), priority)
	}
}

func TestPriorityPreservingWritesAreAccountIsolated(t *testing.T) {
	host := newPriorityHostState()
	rawA, saA := priorityFixture(25000, "uid-a", nil)
	rawB, saB := priorityFixture(77, "uid-b", nil)
	host.files["idx-a"] = rawA
	host.names["idx-a"] = providerName + "-uid-a.json"
	host.files["idx-b"] = rawB
	host.names["idx-b"] = providerName + "-uid-b.json"
	host.install(t)
	if err := disableAuth("idx-a", "id-a", saA, nil, "quota"); err != nil {
		t.Fatal(err)
	}
	if err := syncAuthNote("idx-b", "id-b", saB, nil, false); err != nil {
		t.Fatal(err)
	}
	assertPriorityDocument(t, host.document(t, "idx-a"), 25000)
	assertPriorityDocument(t, host.document(t, "idx-b"), 77)
	if host.saveCount() != 2 {
		t.Fatalf("saves = %d, want 2", host.saveCount())
	}
}

func TestPriorityPreservingWritesAbortOnReadFailure(t *testing.T) {
	host := newPriorityHostState()
	raw, sa := priorityFixture(25000, "read-error-uid", nil)
	host.files["idx-a"] = raw
	host.names["idx-a"] = providerName + "-read-error-uid.json"
	host.getErr["idx-a"] = errors.New("host.auth.get unavailable")
	host.install(t)
	for name, fn := range map[string]func() error{
		"sync-note": func() error { return syncAuthNote("idx-a", "id-a", sa, nil, false) },
		"disable":   func() error { return disableAuth("idx-a", "id-a", sa, nil, "quota") },
		"reenable":  func() error { return reenableAuth("idx-a", "id-a", sa, &creditsSummary{TotalRemain: 100}) },
		"refresh":   func() error { return persistAuthTokens("idx-a", sa) },
		"relogin":   func() error { return persistServiceLoginAuth(sa, serviceCN) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Fatal("expected read failure")
			}
		})
	}
	if host.saveCount() != 0 {
		t.Fatalf("saves = %d, want 0", host.saveCount())
	}
	assertPriorityDocument(t, host.document(t, "idx-a"), 25000)
}

func TestPriorityPreservingWritesAbortOnMalformedRead(t *testing.T) {
	host := newPriorityHostState()
	_, sa := priorityFixture(25000, "malformed-uid", nil)
	host.files["idx-a"] = []byte(`{"priority":25000,"custom":{"keep":true},"broken":`)
	host.names["idx-a"] = providerName + "-malformed-uid.json"
	host.install(t)
	if err := syncAuthNote("idx-a", "id-a", sa, nil, false); err == nil {
		t.Fatal("expected malformed document error")
	}
	if host.saveCount() != 0 {
		t.Fatalf("saves = %d, want 0", host.saveCount())
	}
}

func TestPriorityValueParsing(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
		ok    bool
	}{
		{25000, "25000", true},
		{0, "0", true},
		{-7, "-7", true},
		{" -15 ", "-15", true},
		{float64(12), "12", true},
		{"12.5", "", false},
		{"bad", "", false},
		{nil, "", false},
	} {
		got, ok := priorityAttributeValue(tc.value)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("priorityAttributeValue(%#v) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseAuthPreservesPriorityInMetadataAndAttributes(t *testing.T) {
	for _, priority := range []any{25000, 0, -7, "-15", nil} {
		raw, _ := priorityFixture(priority, "parse-uid", map[string]any{
			"label":        "host label",
			"prefix":       "host-prefix",
			"proxy_url":    "http://proxy.example.invalid",
			"websockets":   true,
			"custom_field": "host-custom",
		})
		reply, err := handleParseAuth(mustJSON(pluginapi.AuthParseRequest{
			Provider: providerName,
			FileName: providerName + "-parse-uid.json",
			Path:     "/auth/" + providerName + "-parse-uid.json",
			RawJSON:  raw,
		}))
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(reply, &env); err != nil {
			t.Fatal(err)
		}
		var resp pluginapi.AuthParseResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.Handled {
			t.Fatal("auth file was not handled")
		}
		value, ok := priorityAttributeValue(resp.Auth.Metadata["priority"])
		want, wantOK := priorityAttributeValue(priority)
		if ok != wantOK || value != want {
			t.Fatalf("metadata priority = (%q, %v), want (%q, %v)", value, ok, want, wantOK)
		}
		attribute := resp.Auth.Attributes["priority"]
		if attribute != want {
			t.Fatalf("attribute priority = %q, want %q", attribute, want)
		}
		if resp.Auth.FileName != providerName+"-parse-uid.json" || resp.Auth.ID != "" {
			t.Fatalf("identity changed: filename=%q id=%q", resp.Auth.FileName, resp.Auth.ID)
		}
		if resp.Auth.Disabled != false {
			t.Fatalf("parse disabled = %v, want false", resp.Auth.Disabled)
		}
		if resp.Auth.Label != "host label" || resp.Auth.Prefix != "host-prefix" || resp.Auth.ProxyURL != "http://proxy.example.invalid" {
			t.Fatalf("parsed routing fields lost: label=%q prefix=%q proxy=%q", resp.Auth.Label, resp.Auth.Prefix, resp.Auth.ProxyURL)
		}
		if resp.Auth.Metadata["custom_field"] != "host-custom" || resp.Auth.Metadata["websockets"] != true {
			t.Fatalf("parsed host metadata lost: %#v", resp.Auth.Metadata)
		}
		if _, ok := resp.Auth.Metadata["auth"]; ok {
			t.Fatal("parsed metadata contains stale auth credentials")
		}
		if _, ok := resp.Auth.Metadata["account"]; ok {
			t.Fatal("parsed metadata contains stale account credentials")
		}
		var saved map[string]json.RawMessage
		if err := json.Unmarshal(resp.Auth.StorageJSON, &saved); err != nil {
			t.Fatal(err)
		}
		if _, ok := saved["custom_field"]; !ok {
			t.Fatalf("StorageJSON lost host metadata: %#v", saved)
		}
		if _, err := strconv.Atoi(attribute); attribute != "" && err != nil {
			t.Fatalf("attribute priority %q is not an integer", attribute)
		}
	}
}

type priorityRoundTripFunc func(*http.Request) (*http.Response, error)

func (f priorityRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func withPriorityRefreshTransport(t *testing.T, body string) {
	t.Helper()
	old := sharedHTTPClient()
	sharedClient = &http.Client{Transport: priorityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() { sharedClient = old })
}

func TestHostRefreshPreservesCurrentHostMetadataAndAttributes(t *testing.T) {
	withPriorityRefreshTransport(t, `{"code":0,"data":{"accessToken":"new-fake","refreshToken":"new-refresh","expiresIn":3600}}`)
	_, sa := priorityFixture(nil, "refresh-uid", nil)
	currentPriority := float64(25000)
	note := "host note"
	disabled := true
	req := authRefreshRequestWire{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:      "refresh-uid",
			StorageJSON: mustJSON(sa),
			Metadata:    map[string]any{"priority": currentPriority, "note": note, "disabled": disabled, "custom": "host-value"},
			Attributes:  map[string]string{"priority": "25000", "path": "/auth/current.json", "account": "refresh-uid"},
		},
	}
	reply, err := handleRefreshAuth(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(reply, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthRefreshResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	got, ok := priorityAttributeValue(resp.Auth.Metadata["priority"])
	if !ok || got != "25000" {
		t.Fatalf("metadata priority = (%q, %v)", got, ok)
	}
	if resp.Auth.Metadata["note"] != note || resp.Auth.Metadata["disabled"] != disabled || resp.Auth.Metadata["custom"] != "host-value" {
		t.Fatalf("host metadata lost: %#v", resp.Auth.Metadata)
	}
	if resp.Auth.Attributes["priority"] != "25000" || resp.Auth.Attributes["path"] != "/auth/current.json" || resp.Auth.Attributes["account"] != "refresh-uid" {
		t.Fatalf("host attributes lost: %#v", resp.Auth.Attributes)
	}
	if resp.Auth.FileName != "" || resp.Auth.ID != "" {
		t.Fatalf("refresh must preserve host identity: filename=%q id=%q", resp.Auth.FileName, resp.Auth.ID)
	}
	if resp.Auth.Disabled != disabled {
		t.Fatalf("refresh disabled = %v, want %v", resp.Auth.Disabled, disabled)
	}
	if _, exists := resp.Auth.Metadata["priority"]; !exists {
		t.Fatalf("refresh priority missing from metadata: %#v", resp.Auth.Metadata)
	}
	if _, ok := resp.Auth.Metadata["auth"]; ok {
		t.Fatal("refresh metadata contains stale auth credentials")
	}
	if _, ok := resp.Auth.Metadata["account"]; ok {
		t.Fatal("refresh metadata contains stale account credentials")
	}
	if rawNote, ok := resp.Auth.Metadata["note"].(string); !ok || rawNote != note {
		t.Fatalf("refresh metadata note = %#v, want %q", resp.Auth.Metadata["note"], note)
	}
	assertRefreshedHostDocument(t, resp)
}

// assertRefreshedHostDocument emulates CPA pluginTokenStorage.RawJSON:
// Metadata overlays the refreshed StorageJSON before the host writes the file.
func assertRefreshedHostDocument(t *testing.T, resp pluginapi.AuthRefreshResponse) {
	t.Helper()
	doc := map[string]any{}
	if err := json.Unmarshal(resp.Auth.StorageJSON, &doc); err != nil {
		t.Fatalf("refreshed StorageJSON invalid: %v", err)
	}
	for key, value := range resp.Auth.Metadata {
		doc[key] = value
	}
	if got, ok := doc["priority"]; !ok {
		t.Fatalf("priority lost from persisted host document: %#v", doc)
	} else if normalized, valid := priorityAttributeValue(got); !valid || normalized != "25000" {
		t.Fatalf("persisted priority = %#v", got)
	}
	if got, ok := doc["custom"].(string); !ok || got != "host-value" {
		t.Fatalf("host custom metadata lost: %#v", doc["custom"])
	}
	if got, ok := doc["note"].(string); !ok || got != "host note" {
		t.Fatalf("host note lost: %#v", doc["note"])
	}
	credentials, ok := doc["auth"].(map[string]any)
	if !ok {
		t.Fatalf("credential container missing: %#v", doc["auth"])
	}
	if got := credentials["accessToken"]; got != "new-fake" {
		t.Fatalf("persisted access token = %#v, want refreshed token", got)
	}
}

func TestHostRefreshPrefersCurrentHostPriorityAndValidAttribute(t *testing.T) {
	withPriorityRefreshTransport(t, `{"code":0,"data":{"accessToken":"new-fake","refreshToken":"new-refresh","expiresIn":3600}}`)
	_, sa := priorityFixture(nil, "refresh-uid", nil)
	req := authRefreshRequestWire{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:      "refresh-uid",
			StorageJSON: mustJSON(sa),
			Metadata:    map[string]any{"priority": 400},
			Attributes:  map[string]string{"priority": "999"},
		},
	}
	reply, err := handleRefreshAuth(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(reply, &env)
	var resp pluginapi.AuthRefreshResponse
	_ = json.Unmarshal(env.Result, &resp)
	if got, _ := priorityAttributeValue(resp.Auth.Metadata["priority"]); got != "400" {
		t.Fatalf("metadata priority = %q, want current host value 400", got)
	}
	if resp.Auth.Attributes["priority"] != "999" {
		t.Fatalf("attribute priority = %q, want current host value 999", resp.Auth.Attributes["priority"])
	}
}

func TestLifecyclePatchKeepsNewestCredentialOnDisk(t *testing.T) {
	host := newPriorityHostState()
	raw, stale := priorityFixture(25000, "stale-uid", nil)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	credentials := doc["auth"].(map[string]any)
	credentials["accessToken"] = "latest-on-disk-token"
	updated, _ := json.Marshal(doc)
	host.files["idx-a"] = updated
	host.names["idx-a"] = "workbuddy-stale-uid.json"
	host.install(t)
	if err := syncAuthNote("idx-a", "id-a", stale, nil, false); err != nil {
		t.Fatal(err)
	}
	saved := host.savedDocument(t)
	stored, err := parseStored(mustJSON(saved))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Auth.AccessToken != "latest-on-disk-token" {
		t.Fatalf("lifecycle overwrote newest token with stale value: %q", stored.Auth.AccessToken)
	}
	assertPriorityDocument(t, saved, 25000)
}

func TestHostLoginPollPreservesPriorityForExistingUID(t *testing.T) {
	host := newPriorityHostState()
	raw, _ := priorityFixture(-7, "poll-uid", nil)
	host.files["idx-a"] = raw
	host.names["idx-a"] = "workbuddy-poll-uid.json"
	host.install(t)
	loggedIn := &storedAuth{
		Auth:    storedTokens{AccessToken: "fresh-login-token", RefreshToken: "fresh-refresh", Domain: domainForService(serviceCN)},
		Account: storedAccount{UID: "poll-uid", Nickname: "new nickname"},
	}
	ad := toAuthDataForLoginPollWithExistingFile(loggedIn)
	if ad.FileName != "workbuddy-poll-uid.json" {
		t.Fatalf("login poll filename = %q", ad.FileName)
	}
	var doc map[string]any
	if err := json.Unmarshal(ad.StorageJSON, &doc); err != nil {
		t.Fatal(err)
	}
	assertPriorityDocument(t, doc, -7)
	if value, _ := priorityAttributeValue(ad.Metadata["priority"]); value != "-7" {
		t.Fatalf("login poll metadata priority = %#v", ad.Metadata["priority"])
	}
	if ad.Attributes["priority"] != "-7" {
		t.Fatalf("login poll attribute priority = %q", ad.Attributes["priority"])
	}
	if credentials := doc["auth"].(map[string]any)["accessToken"]; credentials != "fresh-login-token" {
		t.Fatalf("login poll stored token = %#v", credentials)
	}
}

func TestHandleRefreshAuthHostDocumentKeepsRenewedTokenAndPriority(t *testing.T) {
	withPriorityRefreshTransport(t, `{"code":0,"data":{"accessToken":"new-fake","refreshToken":"new-refresh","expiresIn":3600}}`)
	raw, _ := priorityFixture(-7, "refresh-uid", nil)
	req := authRefreshRequestWire{
		AuthRefreshRequest: pluginapi.AuthRefreshRequest{
			AuthID:      "refresh-uid",
			StorageJSON: raw,
			Metadata: map[string]any{
				"priority": -7,
				"auth":     map[string]any{"accessToken": "old-fake"},
				"custom":   "host-value",
			},
			Attributes: map[string]string{"priority": "-7", "path": "/auth/current.json"},
		},
	}
	reply, err := handleRefreshAuth(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(reply, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.AuthRefreshResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	assertRefreshedHostDocumentWithPriority(t, resp, "-7", "new-fake")
}

func assertRefreshedHostDocumentWithPriority(t *testing.T, resp pluginapi.AuthRefreshResponse, priority, token string) {
	t.Helper()
	doc := map[string]any{}
	if err := json.Unmarshal(resp.Auth.StorageJSON, &doc); err != nil {
		t.Fatalf("refreshed StorageJSON invalid: %v", err)
	}
	for key, value := range resp.Auth.Metadata {
		doc[key] = value
	}
	if got, ok := doc["priority"]; !ok {
		t.Fatalf("priority lost from refreshed file: %#v", doc)
	} else if normalized, valid := priorityAttributeValue(got); !valid || normalized != priority {
		t.Fatalf("persisted priority = %#v, want %q", got, priority)
	}
	if got := doc["custom"]; got != "host-value" {
		t.Fatalf("custom host metadata = %#v", got)
	}
	credentials, ok := doc["auth"].(map[string]any)
	if !ok {
		t.Fatalf("credential container missing: %#v", doc["auth"])
	}
	if got := credentials["accessToken"]; got != token {
		t.Fatalf("persisted access token = %#v, want %q", got, token)
	}
	if resp.Auth.Attributes["priority"] != priority {
		t.Fatalf("attribute priority = %q, want %q", resp.Auth.Attributes["priority"], priority)
	}
}
