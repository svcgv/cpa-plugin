package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRewriteSystemInPlaceSanitizesTopLevelSystem covers the Anthropic-protocol
// shape Claude Code sends: a top-level "system" array whose first block is the
// billing header and whose identity block names the Claude Agent SDK. Scanning
// only "messages" let these reach the upstream and produced 11128.
func TestRewriteSystemInPlaceSanitizesTopLevelSystem(t *testing.T) {
	CC := "cc" + "_"
	raw := `{"model":"m","system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: ` + CC + `version=2.1.325.ee6; ` + CC + `entrypoint=sdk-cli;"},` +
		`{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK tool."},` +
		`{"type":"text","text":"Keep this instruction."}],"messages":[{"role":"user","content":"hi"}]}`

	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	rewriteSystemInPlace(obj)

	out, _ := json.Marshal(obj)
	text := string(out)
	if strings.Contains(text, "x-anthropic-billing-header") {
		t.Fatalf("billing header survived: %s", text)
	}
	if strings.Contains(text, "Claude Agent SDK.") {
		t.Fatalf("SDK identity survived unrewritten: %s", text)
	}
	if !strings.Contains(text, "Claude Agent SDK tool.") {
		t.Fatalf("SDK identity rewrite target missing: %s", text)
	}
	if !strings.Contains(text, "Keep this instruction.") {
		t.Fatalf("caller content lost: %s", text)
	}
}

// TestRewriteSystemInPlaceSanitizesStringSystem covers OpenAI-shaped clients
// that send "system" as a plain string on the same request.
func TestRewriteSystemInPlaceSanitizesStringSystem(t *testing.T) {
	obj := map[string]any{
		"system":   "You are Claude Code, Anthropic's official CLI for Claude.",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if !rewriteSystemInPlace(obj) {
		t.Fatal("expected the string system field to be rewritten")
	}
	if got := obj["system"].(string); strings.Contains(got, "official CLI for Claude.") {
		t.Fatalf("identity survived: %q", got)
	}
}

// TestRewriteSystemInPlaceLeavesCleanSystemAlone guards against over-touching.
func TestRewriteSystemInPlaceLeavesCleanSystemAlone(t *testing.T) {
	obj := map[string]any{
		"system":   []any{map[string]any{"type": "text", "text": "Ordinary system text."}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if rewriteSystemInPlace(obj) {
		t.Fatal("clean system payload should not be modified")
	}
}
