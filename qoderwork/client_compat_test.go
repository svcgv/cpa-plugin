package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func testCatalogAuth(t *testing.T, model, display string, contextWindow, maxOutput int) (*storedAuth, string) {
	t.Helper()
	t.Setenv("QODERWORK_CACHE_DIR", t.TempDir())
	sa := &storedAuth{
		Auth:    storedTokens{Region: "cn", PersonalToken: "pt-test", AccessToken: "jt-test", MachineID: "machine-test"},
		Account: storedAccount{UID: "uid-test", Nickname: "Tester"},
	}
	raw := []byte(`{"assistant":[{"key":"` + model + `","display_name":"` + display + `","enable":true,"format":"openai","is_vl":true,"is_reasoning":true,"max_output_tokens":` + jsonInt(maxOutput) + `,"context_config":{"default":{"token_count":` + jsonInt(contextWindow) + `,"is_default":true}}}]}`)
	storeCatalogMetadata(sa, raw)
	return sa, model
}

func jsonInt(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestQoderWireMatchesCLI1189Shape(t *testing.T) {
	sa, model := testCatalogAuth(t, "qfmodel", "Qwen Test", 1_000_000, 65536)
	limit := 4096
	req := &openAIRequest{
		Model:           model,
		ClientSessionID: "session-cli-wire",
		Messages: []openAIMessage{
			{Role: "system", Content: "You are Codex, a coding agent running in the Codex CLI."},
			{Role: "user", Content: "inspect"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"call-1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}]`)},
			{Role: "tool", ToolCallID: "call-1", Content: "/repo"},
		},
		Tools:           json.RawMessage(`[{"type":"function","function":{"name":"shell","description":"Run","parameters":{"type":"object"}}}]`),
		ToolChoice:      json.RawMessage(`{"type":"function","function":{"name":"shell"}}`),
		MaxTokens:       &limit,
		ReasoningEffort: "high",
	}
	raw, err := buildQoderBody(req, model, "ignored", sa)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"chat_prompt", "code_language", "image_urls"} {
		if _, found := body[key]; found {
			t.Fatalf("legacy field %s leaked into CLI 1.1.89 body", key)
		}
	}
	if body["chat_task"] != "FREE_INPUT" || body["version"] != "3" || body["task_id"] != "common" || body["system"] != nil {
		t.Fatalf("CLI envelope mismatch: %#v", body)
	}
	if body["is_reply"] != true || body["is_retry"] != false || body["source"] != float64(1) {
		t.Fatalf("CLI reply fields mismatch: %#v", body)
	}
	params := body["parameters"].(map[string]any)
	if params["max_tokens"] != float64(4096) || params["context_length"] != float64(1_000_000) || params["tool_choice"] != "shell" {
		t.Fatalf("parameters = %#v", params)
	}
	config := body["model_config"].(map[string]any)
	if config["key"] != model || config["display_name"] != "Qwen Test" || config["max_input_tokens"] != float64(1_000_000) || config["is_vl"] != true {
		t.Fatalf("model_config = %#v", config)
	}
	if _, found := config["context_config"]; found {
		t.Fatalf("captured context_config template leaked: %#v", config)
	}
	context := body["chat_context"].(map[string]any)
	if context["text"] != "inspect" {
		t.Fatalf("chat_context text = %#v", context["text"])
	}
	extra := context["extra"].(map[string]any)
	if _, ok := extra["originalContent"].(string); !ok {
		t.Fatalf("originalContent must be a string: %#v", extra["originalContent"])
	}
}

func TestApplyCosyHeadersUsesCLI1189RoutingNames(t *testing.T) {
	sa, model := testCatalogAuth(t, "qfmodel", "Qwen Test", 200_000, 32000)
	req := &openAIRequest{
		Messages:        []openAIMessage{{Role: "user", Content: "hello"}},
		ClientSessionID: "session-header",
	}
	raw, err := buildQoderBody(req, model, "", sa)
	if err != nil {
		t.Fatal(err)
	}
	encoded := qoderEncode(raw)
	httpReq, _ := http.NewRequest(http.MethodPost, chatEndpointFor(sa), strings.NewReader(encoded))
	if err := applyCosyHeaders(httpReq, sa, encoded, chatReqURLForTest(sa), model, true); err != nil {
		t.Fatal(err)
	}
	if httpReq.Header.Get("X-Qoder-Model") != "Qwen Test" {
		t.Fatalf("X-Qoder-Model = %q", httpReq.Header.Get("X-Qoder-Model"))
	}
	if httpReq.Header.Get("X-Session-ID") == "" || httpReq.Header.Get("X-Qoder-Session-Id") == "" || httpReq.Header.Get("X-RequestSet-ID") == "" || httpReq.Header.Get("X-Request-Id") == "" {
		t.Fatalf("CLI routing headers missing: %#v", httpReq.Header)
	}
	if httpReq.Header.Get("Cosy-Version") != qoderCLIWireVersion {
		t.Fatalf("Cosy-Version = %q", httpReq.Header.Get("Cosy-Version"))
	}
	if httpReq.Header.Get("User-Agent") != "qoder/"+qoderCLIWireVersion {
		t.Fatalf("User-Agent = %q", httpReq.Header.Get("User-Agent"))
	}
	if httpReq.Header.Get("x-model-key") != "" || httpReq.Header.Get("x-model-source") != "" {
		t.Fatalf("legacy routing headers leaked: %#v", httpReq.Header)
	}
}

func chatReqURLForTest(sa *storedAuth) string {
	return chatEndpointFor(sa)
}

func TestCodexResponsesReasoningAndCustomToolItems(t *testing.T) {
	payload := []byte(`{
		"model":"qfmodel",
		"input":[
			{"role":"system","content":[{"type":"input_text","text":"You are Codex, a coding agent running in the Codex CLI."}]},
			{"role":"user","content":[{"type":"input_text","text":"fix it"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"Need to inspect files."}]},
			{"type":"custom_tool_call","call_id":"call-custom","name":"shell","input":{"command":"pwd"}},
			{"type":"custom_tool_call_output","call_id":"call-custom","output":"/repo"}
		]
	}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatal(err)
	}
	messages := req.effectiveMessages()
	if len(messages) != 5 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[2].ReasoningContent != "Need to inspect files." {
		t.Fatalf("reasoning = %#v", messages[2])
	}
	if messages[3].Role != "assistant" || !strings.Contains(string(messages[3].ToolCalls), `"name":"shell"`) {
		t.Fatalf("custom tool call = %#v", messages[3])
	}
	if messages[4].Role != "tool" || messages[4].ToolCallID != "call-custom" || messages[4].Content != "/repo" {
		t.Fatalf("custom tool output = %#v", messages[4])
	}
}

func TestClaudeCodeAnthropicContentBlocks(t *testing.T) {
	payload := []byte(`{"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[
		{"role":"user","content":[
			{"type":"text","text":"result is next"},
			{"type":"tool_result","tool_use_id":"call-claude","content":[{"type":"text","text":"ok"}],"is_error":false}
		]}
	]}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatal(err)
	}
	messages := requestMessages(&req)
	if len(messages) != 3 {
		t.Fatalf("expanded messages = %#v", messages)
	}
	if messages[1].Role != "user" || messages[1].Content != "result is next" {
		t.Fatalf("text segment = %#v", messages[1])
	}
	if messages[2].Role != "tool" || messages[2].ToolCallID != "call-claude" || messages[2].Content != "ok" {
		t.Fatalf("tool result = %#v", messages[2])
	}
	body, err := buildQoderBody(&req, "qfmodel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "official CLI for Claude") {
		t.Fatalf("blocked Claude fingerprint leaked: %s", body)
	}
}

func TestDeepSeekHarnessDeveloperAndToolHistory(t *testing.T) {
	payload := []byte(`{
		"model":"qfmodel",
		"messages":[
			{"role":"developer","content":[{"type":"text","text":"You are an AI agent powered by DeepSeek Harness."}]},
			{"role":"user","content":[{"type":"text","text":"read main.go"}]},
			{"role":"assistant","content":"","reasoning_content":"plan","tool_calls":[{"id":"call-ds","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},
			{"role":"tool","tool_call_id":"call-ds","content":[{"type":"text","text":"package main"}]},
			{"role":"user","content":"summarize"}
		],
		"tools":[{"type":"function","function":{"name":"read_file","description":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]
	}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatal(err)
	}
	body, err := buildQoderBody(&req, "qfmodel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Messages []map[string]any `json:"messages"`
		Tools    []any            `json:"tools"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Messages) != 5 || parsed.Messages[0]["role"] != "system" {
		t.Fatalf("developer was not folded to system: %#v", parsed.Messages)
	}
	if strings.Contains(string(body), "DeepSeek Harness") {
		t.Fatalf("blocked Harness fingerprint leaked: %s", body)
	}
	calls, ok := parsed.Messages[2]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool call lost: %#v", parsed.Messages[2])
	}
	if len(parsed.Tools) != 1 {
		t.Fatalf("tools = %#v", parsed.Tools)
	}
}

func TestClaudeAssistantTextAndToolUseStayOnOneMessage(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[
		{"type":"text","text":"I will inspect it."},
		{"type":"tool_use","id":"call-read","name":"Read","input":{"path":"main.go"}},
		{"type":"thinking","thinking":"Need the file.","signature":"sig-1"}
	]}]}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatal(err)
	}
	messages := req.effectiveMessages()
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "assistant" || messages[0].Content != "I will inspect it." || !strings.Contains(string(messages[0].ToolCalls), `"name":"Read"`) {
		t.Fatalf("text/tool assistant = %#v", messages[0])
	}
	if messages[1].ReasoningContent != "Need the file." || messages[1].ReasoningSig != "sig-1" {
		t.Fatalf("thinking assistant = %#v", messages[1])
	}
}

func TestQoderAutoCompactThresholdMatchesCLI(t *testing.T) {
	for _, tc := range []struct {
		window, output, want int
	}{
		{200_000, 32_000, 167_000},
		{1_000_000, 32_000, 931_000},
	} {
		t.Setenv("QODER_AUTOCOMPACT_WINDOW", "")
		t.Setenv("QODER_AUTOCOMPACT_PCT_OVERRIDE", "")
		if got := qoderAutoCompactThreshold(int64(tc.window), tc.output, 0); got != tc.want {
			t.Errorf("window=%d: got %d want %d", tc.window, got, tc.want)
		}
	}
}
