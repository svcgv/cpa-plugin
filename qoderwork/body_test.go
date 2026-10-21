package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Codex reaches the plugin through CPA's /responses bridge, which sends
// content as a list of typed parts rather than a plain string.
func TestOpenAIRequestParsesContentParts(t *testing.T) {
	payload := []byte(`{
		"model":"qfmodel",
		"messages":[
			{"role":"system","content":[{"type":"input_text","text":"be brief"}]},
			{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_text","text":"world"}]},
			{"role":"assistant","content":"plain string"}
		]
	}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if req.Messages[0].Content != "be brief" {
		t.Fatalf("system content = %q", req.Messages[0].Content)
	}
	if req.Messages[1].Content != "hello\nworld" {
		t.Fatalf("user content = %q", req.Messages[1].Content)
	}
	if req.Messages[2].Content != "plain string" {
		t.Fatalf("assistant content = %q", req.Messages[2].Content)
	}
	if got := extractLatestUserPrompt(req.Messages); got != "hello\nworld" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestOpenAIRequestParsesOutputTextAndNestedContent(t *testing.T) {
	payload := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"output_text","text":"from output"},
			{"type":"message","content":[{"type":"text","text":"nested"}]},
			{"type":"input_image","image_url":"https://example.com/a.png"}
		]}
	]}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := req.Messages[0].Content
	if got != "from output\nnested" {
		t.Fatalf("content = %q", got)
	}
}

// The upstream body still has to be buildable from the parsed prompt.
func TestBuildQoderBodyWithContentParts(t *testing.T) {
	req := &openAIRequest{Model: "qfmodel", Messages: []openAIMessage{{
		Role:    "user",
		Content: "ping",
	}}}
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	if !strings.Contains(string(body), `"key":"qfmodel"`) {
		t.Fatalf("model key missing in body")
	}
}

// Codex ships its own system prompt. The Qoder template must not compete with
// it, otherwise the model answers "I am Qoder" and uses Qoder's tool names.
func TestClientSystemPromptReplacesQoderTemplate(t *testing.T) {
	payload := []byte(`{"model":"qfmodel","messages":[
		{"role":"system","content":"You are Codex, an OpenAI coding agent."},
		{"role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)
	var req openAIRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatal(err)
	}
	body, err := buildQoderBody(&req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d want 2 (client system + user)", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are Codex, an OpenAI coding agent." {
		t.Fatalf("first message = %#v", first)
	}
	if len(msgs) > 1 {
		if role := msgs[1].(map[string]any)["role"]; role != "user" {
			t.Fatalf("second message role = %v", role)
		}
	}
	joined, _ := json.Marshal(msgs)
	for _, banned := range []string{"You are Qoder", "D:/Projects/Qoder", "MINGW64", "TodoWrite"} {
		if strings.Contains(string(joined), banned) {
			t.Fatalf("template leaked %q into a Codex request", banned)
		}
	}
}

// Requests without a client system prompt keep the Qoder template (the
// gateway expects that shape) but with a rewritten <env> block.
func TestTemplateEnvIsRewrittenForThisHost(t *testing.T) {
	req := &openAIRequest{Model: "qfmodel", Messages: []openAIMessage{{Role: "user", Content: "hi"}}}
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if strings.Contains(s, "D:/Projects/Qoder") || strings.Contains(s, "MINGW64") {
		t.Fatal("stale template environment leaked")
	}
	if !strings.Contains(s, "Working directory: unknown") {
		t.Fatal("env block must not claim a concrete directory")
	}
	// The plugin runs inside CPA, so a host path (e.g. /CLIProxyAPI) must never
	// be presented to the model as the user's project.
	if wd, err := os.Getwd(); err == nil && wd != "" && strings.Contains(s, "Working directory: "+wd) {
		t.Fatalf("host cwd %q leaked into the prompt", wd)
	}
	if strings.Contains(s, "Today's date: <no value>") {
		t.Fatal("date placeholder leaked")
	}
}

// When the client discloses its real working directory, the template must use
// it instead of the (irrelevant) CPA host directory.
func TestTemplateEnvUsesClientWorkingDirectory(t *testing.T) {
	req := &openAIRequest{
		Model:    "qfmodel",
		Messages: []openAIMessage{{Role: "user", Content: "hi"}},
		Metadata: map[string]any{"cwd": "/Users/someone/my-project"},
	}
	body, err := buildQoderBody(req, "qfmodel", "personal_professional_trial")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Working directory: /Users/someone/my-project") {
		t.Fatal("client cwd not used in env block")
	}
}

func TestClientWorkingDirForms(t *testing.T) {
	for name, req := range map[string]*openAIRequest{
		"top-level": {CWD: "/a"},
		"metadata":  {Metadata: map[string]any{"cwd": "/b"}},
		"snake":     {Metadata: map[string]any{"working_directory": "/c"}},
		"camel":     {Metadata: map[string]any{"workingDirectory": "/d"}},
		"none":      {},
	} {
		if got := req.clientWorkingDir(); got == "" && name != "none" {
			t.Fatalf("%s: empty", name)
		}
	}
	if (&openAIRequest{}).clientWorkingDir() != "" {
		t.Fatal("expected empty for no disclosure")
	}
}
