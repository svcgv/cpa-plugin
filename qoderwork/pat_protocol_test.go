package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPATBodyPreservesToolsAndResponsesHistory(t *testing.T) {
	const payload = `{"model":"auto","input":[{"role":"user","content":"check"},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"q\":1}"},{"type":"function_call_output","call_id":"call-1","output":"found"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"max_output_tokens":1024}`
	var req openAIRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		t.Fatal(err)
	}
	body, err := buildQoderBody(&req, "auto", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages   []map[string]any `json:"messages"`
		Tools      []any            `json:"tools"`
		Parameters map[string]any   `json:"parameters"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Parameters["max_tokens"] != float64(1024) {
		t.Fatalf("tools or max_tokens lost: %#v", got.Parameters)
	}
	var call, result bool
	for _, m := range got.Messages {
		if m["role"] == "assistant" && m["tool_calls"] != nil {
			call = true
		}
		if m["role"] == "tool" && m["tool_call_id"] == "call-1" && m["content"] == "found" {
			result = true
		}
	}
	if !call || !result {
		t.Fatalf("tool call/result missing: %s", body)
	}
}

func TestContextOptionsAndCatalogIsolation(t *testing.T) {
	t.Setenv("QODERWORK_CACHE_DIR", t.TempDir())
	defer setContextWindows(true, true)
	cn := &storedAuth{Auth: storedTokens{PersonalToken: "pt-cn-test", Region: "cn"}, Account: storedAccount{UID: "same"}}
	intl := &storedAuth{Auth: storedTokens{PersonalToken: "pt-intl-test", Region: "intl"}, Account: storedAccount{UID: "same"}}
	const raw = `{"assistant":[{"key":"test-model","source":"system","enable":true,"context_config":{"small":{"token_count":200000,"is_default":true},"large":{"token_count":1000000}}}]}`
	storeCatalogMetadata(cn, []byte(raw))
	saveCatalog(cn, []byte(raw))
	if len(loadCatalog(intl)) != 0 {
		t.Fatal("cross-region cache leak")
	}
	setContextWindows(true, true)
	models := loadCatalog(cn)
	if len(models) != 1 || models[0].ContextLength != 1000000 {
		t.Fatalf("max context: %#v", models)
	}
	base := map[string]any{"model_config": map[string]any{}}
	applyModelMetadata(base, cn, "test-model")
	config := base["model_config"].(map[string]any)["context_config"].(map[string]any)
	if config["small"].(map[string]any)["is_default"] != true || config["large"].(map[string]any)["is_default"] != false {
		t.Fatal("provider's context defaults were not preserved on wire")
	}
	setContextWindows(false, true)
	models = loadCatalog(cn)
	if models[0].ContextLength != 200000 {
		t.Fatalf("default context: %#v", models)
	}
}

func TestCampaignWindowBelongsToStartDate(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, campaignChinaZone)
	c := rewardCampaign{StartAt: now.Add(-23 * time.Hour).Unix(), EndAt: now.Add(time.Hour).Unix()}
	if campaignToday(c, now) {
		t.Fatal("yesterday's window counted as today")
	}
	c.StartAt = now.Add(-time.Hour).Unix()
	c.EndAt = now.Add(time.Hour).Unix()
	if !campaignToday(c, now) {
		t.Fatal("today's window not recognized")
	}
}

func TestInternationalCampaignUsesItsOwnHost(t *testing.T) {
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "openapi.qoder.sh" || r.Header.Get("Cosy-Clienttype") != "10" {
			t.Errorf("wrong campaign endpoint/header: %s", r.URL.Host)
		}
		_, _ = w.Write([]byte(`{"campaigns":[]}`))
	})
	sa := &storedAuth{Auth: storedTokens{Region: "intl", PersonalToken: "pt-test", AccessToken: "jt-test"}}
	if _, err := fetchCampaigns(context.Background(), sa); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyAccountWithoutPATDoesNotCheckIn(t *testing.T) {
	server := qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("legacy account made a reward request") })
	sa := &storedAuth{Auth: storedTokens{Region: "cn", AccessToken: "dt-old"}}
	steps, _, success, attention, wrote := runQoderRewards(context.Background(), sa, rewardActionOrder, true)
	if !success || attention || wrote || len(steps) != 1 || steps[0].Status != "skipped" || len(server.Paths()) != 0 {
		t.Fatalf("unexpected reward behavior: %#v", steps)
	}
}

func TestCredentialStringsAreRedacted(t *testing.T) {
	input := "pt-abcdefghijklmnopqrst jt-abcdefghijklmnopqrst"
	if strings.Contains(redactSecrets(input), "abcdefghijkl") {
		t.Fatal("token leaked")
	}
}

func TestConcurrentPATExchangeIsShared(t *testing.T) {
	var requests atomic.Int32
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"token":"jt-refreshed","expires_in":3600000}`))
	})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sa := &storedAuth{Auth: storedTokens{PersonalToken: "pt-singleflight-unique", Region: "cn"}}
			errs <- preparePATCredential(sa)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("PAT was exchanged %d times", got)
	}
}

func TestChat401RefreshesPATOnce(t *testing.T) {
	var chatRequests, exchanges int
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			exchanges++
			_, _ = w.Write([]byte(`{"token":"jt-fresh","expires_in":3600000}`))
		case "/chat":
			chatRequests++
			if chatRequests == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	sa := &storedAuth{Auth: storedTokens{PersonalToken: "pt-refresh-after-401", AccessToken: "jt-old"}, Account: storedAccount{UID: "uid-401"}}
	req, _ := http.NewRequest(http.MethodPost, "https://gateway.qoder.com.cn/chat", strings.NewReader("encoded"))
	stream, status, _, err := openSignedChatStream(req, sa, "encoded", "auto")
	if err != nil || status != 200 {
		t.Fatalf("retry status=%d err=%v", status, err)
	}
	stream.Close()
	if chatRequests != 2 || exchanges != 1 || sa.Auth.AccessToken != "jt-fresh" {
		t.Fatalf("retry calls=%d exchange=%d", chatRequests, exchanges)
	}
}

func TestInternationalPATCatalogAndChat(t *testing.T) {
	t.Setenv("QODERWORK_CACHE_DIR", t.TempDir())
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			if r.URL.Host != "openapi.qoder.sh" {
				t.Error("wrong token host")
			}
			_, _ = w.Write([]byte(`{"token":"jt-test","expires_in":3600000}`))
		case "/api/v1/userinfo":
			if r.URL.Host != "openapi.qoder.sh" {
				t.Error("wrong identity host")
			}
			_, _ = w.Write([]byte(`{"id":"same","name":"Test"}`))
		case "/algo/api/v2/model/list":
			if r.URL.Host != "api3.qoder.sh" {
				t.Error("wrong model host")
			}
			_, _ = w.Write([]byte(`{"assistant":[{"key":"cmodel","display_name":"Cantus","enable":true}]}`))
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			if r.URL.Host != "api3.qoder.sh" || r.Header.Get("x-model-key") != "cmodel" {
				t.Error("wrong chat routing")
			}
			_, _ = w.Write([]byte("data:{\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\ndata:{\"body\":\"[DONE]\"}\n\n"))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	})
	tok, err := exchangePATForJobTokenAt(qoderIntlOpenAPI, "pt-international-test")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := fetchUserInfoAt(qoderIntlOpenAPI, tok.Token)
	if err != nil {
		t.Fatal(err)
	}
	sa := buildStoredAuthFromJobToken("pt-international-test", tok, ui)
	sa.Auth.Region = "intl"
	sa.Auth.Domain = qoderDomainIntl
	models, err := callModelsForAuth(sa)
	if err != nil || len(models) != 1 || models[0].ID != "cmodel" {
		t.Fatalf("catalog: %#v %v", models, err)
	}
	storage, _ := json.Marshal(sa)
	payload := []byte(`{"model":"cmodel","messages":[{"role":"user","content":"hello"}]}`)
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Model: "cmodel", StorageJSON: storage, Payload: payload})
	result, err := handleExecExecute(raw)
	if err != nil || len(result) == 0 {
		t.Fatalf("chat: %v", err)
	}
	if _, err = resolveRequestModel(sa, "unknown", nil); err == nil {
		t.Fatal("unknown model was accepted")
	}
}

func TestDirectStreamDoesNotWaitForConnectionClose(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data:{\"body\":\"[DONE]\"}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, _, _, err := hostHTTPDoStreamDirect(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	done := make(chan error, 1)
	go func() { _, err := aggregateQoderSSE(newHostStreamReader(stream), "test"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream waited for connection close")
	}
}
