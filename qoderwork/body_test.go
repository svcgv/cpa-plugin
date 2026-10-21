package main

import (
	"encoding/json"
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
