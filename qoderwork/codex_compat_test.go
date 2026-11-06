package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLiveCatalogReasoningHints(t *testing.T) {
	path := os.Getenv("QODER_TEST_PAT_PATH")
	if path == "" {
		t.Skip("no PAT path")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read PAT")
	}
	pat := strings.TrimSpace(string(b))
	tok, err := exchangePATForJobToken(pat)
	if err != nil {
		t.Fatal("token exchange failed")
	}
	ui, err := fetchUserInfo(tok.Token)
	if err != nil {
		t.Fatal("userinfo failed")
	}
	sa := buildStoredAuthFromJobToken(pat, tok, ui)
	models, err := callModelsForAuth(sa)
	if err != nil {
		t.Fatalf("catalog failed: %v", err)
	}
	for _, m := range models {
		hint, ok := metadataForModel(sa, m.ID)
		if !ok {
			continue
		}
		for k, v := range hint.ThinkingConfig {
			var levels []string
			for name := range v.Efforts {
				levels = append(levels, name)
			}
			t.Logf("id=%s name=%s thinking=%s default=%t effort_levels=%v", m.ID, m.Name, k, v.IsDefault, levels)
		}
	}
}

func TestCompatibleReasoningUsesDeclaredLevels(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "test-compatible-reasoning"}}
	storeCatalogMetadata(sa, []byte(`{"assistant":[{"key":"qfmodel","display_name":"Qwen3.8-Flash","enable":true,"thinking_config":{"enabled":{"is_default":true,"efforts":{"low":{},"medium":{},"xhigh":{}}},"disabled":{"is_default":false}}},{"key":"undeclared","enable":true}]}`))
	for _, tc := range []struct {
		model, effort, want string
		enabled             bool
	}{{"qfmodel", "high", "xhigh", true}, {"qfmodel", "xhigh", "xhigh", true}, {"qfmodel", "medium", "medium", true}, {"undeclared", "high", "", false}, {"undeclared", "", "", false}} {
		got, toggle, err := resolveReasoning(sa, tc.model, tc.effort)
		if err != nil || got != tc.want || (toggle != nil) != tc.enabled {
			t.Errorf("%s/%s -> %q %v %v", tc.model, tc.effort, got, toggle, err)
		}
	}
	req := &openAIRequest{Messages: []openAIMessage{{Role: "user", Content: "hello"}}, ReasoningEffort: "high"}
	raw, err := buildQoderBody(req, "qfmodel", "test", sa)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Parameters map[string]any `json:"parameters"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Parameters["reasoning_effort"] != "xhigh" {
		t.Fatal("high not mapped to supported xhigh")
	}
}

func TestReferenceQueueIdentityAndContextConfiguration(t *testing.T) {
	defer setContextWindows(true, true)
	setContextWindows(true, true)
	sa := &storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "test-reference-context"}}
	const catalog = `{"assistant":[{"key":"qfmodel","display_name":"Qwen3.8-Flash","enable":true,"max_output_tokens":32768,"context_config":{"small":{"token_count":200000,"is_default":true},"large":{"token_count":1000000,"is_default":false}}}]}`
	storeCatalogMetadata(sa, []byte(catalog))
	limit := 70000
	req := &openAIRequest{Messages: []openAIMessage{{Role: "user", Content: "hello"}}, MaxTokens: &limit}
	encoded, err := buildQoderBody(req, "qfmodel", "test", sa)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		RequestSetID string `json:"request_set_id"`
		Business     struct {
			ID string `json:"id"`
		} `json:"business"`
		Parameters struct {
			MaxTokens int `json:"max_tokens"`
		} `json:"parameters"`
		ModelConfig struct {
			ContextConfig map[string]struct {
				IsDefault bool `json:"is_default"`
			} `json:"context_config"`
		} `json:"model_config"`
	}
	if err = json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestSetID == "" || body.Business.ID != body.RequestSetID {
		t.Fatalf("business.id must equal request_set_id")
	}
	if body.Parameters.MaxTokens != 32768 {
		t.Fatalf("declared output limit ignored: %d", body.Parameters.MaxTokens)
	}
	if !body.ModelConfig.ContextConfig["small"].IsDefault || body.ModelConfig.ContextConfig["large"].IsDefault {
		t.Fatal("provider context defaults were rewritten")
	}
}

func TestReferenceSessionIdentityUsesAccountModelAndClientSession(t *testing.T) {
	cn := &storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "account-a"}}
	other := &storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "account-b"}}
	first := referenceSessionID(cn, "qfmodel", "session-1")
	if first != referenceSessionID(cn, "qfmodel", "session-1") {
		t.Fatal("same client session changed")
	}
	if first == referenceSessionID(cn, "qfmodel", "session-2") || first == referenceSessionID(cn, "dmodel", "session-1") || first == referenceSessionID(other, "qfmodel", "session-1") {
		t.Fatal("session isolation lost")
	}
	if referenceSessionID(cn, "qfmodel", "") == referenceSessionID(cn, "qfmodel", "") {
		t.Fatal("unidentified conversations share a session")
	}
}

func TestCatalogPublishesReadableIDAndResolvesUpstreamKey(t *testing.T) {
	const body = `{"assistant":[{"key":"qfmodel","display_name":"Qwen3.8-Flash","enable":true}]}`
	models, err := parseModelListResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	models = publicModelCatalog(models)
	if len(models) != 1 || models[0].ID != "qoder/qwen3.8-flash" {
		t.Fatalf("catalog did not publish readable ID: %#v", models)
	}
	sa := &storedAuth{Auth: storedTokens{Region: "cn", PersonalToken: "pt-test"}, Account: storedAccount{UID: "model-readable"}}
	storeCatalogMetadata(sa, []byte(body))
	for _, id := range []string{"qoder/qwen3.8-flash", "qfmodel"} {
		key, err := resolveRequestModel(sa, id, nil)
		if err != nil || key != "qfmodel" {
			t.Fatalf("model %q resolved to %q: %v", id, key, err)
		}
	}
}

func TestReadableNamesRemainIsolatedAndHandleCollisions(t *testing.T) {
	cn := &storedAuth{Auth: storedTokens{Region: "cn", PersonalToken: "pt-test-cn"}, Account: storedAccount{UID: "same-id"}}
	intl := &storedAuth{Auth: storedTokens{Region: "intl", PersonalToken: "pt-test-intl"}, Account: storedAccount{UID: "same-id"}}
	const cnBody = `{"assistant":[{"key":"mmodel","display_name":"MiniMax M3.1","enable":true},{"key":"other","display_name":"MiniMax M3.1","enable":true}]}`
	const intlBody = `{"assistant":[{"key":"mmodel","display_name":"MiniMax M4","enable":true}]}`
	storeCatalogMetadata(cn, []byte(cnBody))
	storeCatalogMetadata(intl, []byte(intlBody))
	models, err := parseModelListResponse([]byte(cnBody))
	if err != nil {
		t.Fatal(err)
	}
	models = publicModelCatalog(models)
	if len(models) != 1 || models[0].ID != "qoder/minimax-m3.1" {
		t.Fatalf("duplicate slug: %#v", models)
	}
	if key, err := resolveRequestModel(cn, "qoder/minimax-m3.1", nil); err != nil || key != "mmodel" {
		t.Fatalf("CN mapping %q %v", key, err)
	}
	if _, ok := metadataForModel(intl, "qoder/minimax-m3.1"); ok {
		t.Fatal("CN model leaked into international index")
	}
	if key, err := resolveRequestModel(intl, "qoder/minimax-m4", nil); err != nil || key != "mmodel" {
		t.Fatalf("INTL mapping %q %v", key, err)
	}
}
