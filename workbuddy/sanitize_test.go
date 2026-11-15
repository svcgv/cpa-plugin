package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeBlockedTemplates(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"Claude Code identity", "You are Claude Code, Anthropic's official CLI tool for Claude.", "You are Claude Code, Anthropic's official CLI tool for Claude."},
		{"default branch", "Default branch (you will usually use this for PRs)", "Default branch (you will usually use this for PRs)"},
		{"Codex CLI identity", "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.", "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."},
		{"feedback URL", "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
		{"partial template unchanged", "    You are Claude Code, but this is not the blocked template", "    You are Claude Code, but this is not the blocked template"},
		{"ordinary cc assignment", "Set cc_mode=display; want: other", "Set cc_mode=display; want: other"},
		{"unrelated Anthropic URL", "prefix https://github.com/anthropics/example suffix", "prefix https://github.com/anthropics/example suffix"},
		{"no fingerprint", "Hello world", "Hello world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBlockedTemplates(tt.in); got != tt.want {
				t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPrepareUpstreamBodySanitizesFingerprintsAndPreservesReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"other-model","reasoning_effort":"low","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; keep me"}]}`)
	out := prepareUpstreamBody(body, nil, nil, "other-model")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", obj["reasoning_effort"])
	}
	messages := obj["messages"].([]any)
	content := messages[0].(map[string]any)["content"].(string)
	if strings.Contains(strings.ToLower(content), "x-anthropic-billing-header") || strings.Contains(strings.ToLower(content), "cc_") {
		t.Fatalf("fingerprints remain: %q", content)
	}
	if !strings.Contains(content, "keep me") {
		t.Fatalf("semantic text lost: %q", content)
	}
}

func TestPrepareUpstreamBodyPreservesCallerReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning_effort":"medium","messages":[]}`)
	out := prepareUpstreamBody(body, nil, nil, "serve-beta")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "serve-beta" || obj["reasoning_effort"] != "medium" {
		t.Fatalf("rewritten payload = %#v", obj)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("short string should be unchanged")
	}
	if truncate("hello world", 5) != "hello" {
		t.Fatal("should truncate to 5 chars")
	}
	if truncate("", 5) != "" {
		t.Fatal("empty string")
	}
}
