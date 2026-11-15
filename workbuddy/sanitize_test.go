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
		// in 是 harness 注入的原文；want 是改写后的串。
		{
			name: "Claude Code identity rewrites original",
			in:   "You are Claude Code, Anthropic's official CLI for Claude.",
			want: "You are Claude Code, Anthropic's official CLI tool for Claude.",
		},
		{
			name: "Claude Agent SDK identity rewrites original",
			in:   "You are a Claude agent, built on Anthropic's Claude Agent SDK.",
			want: "You are a Claude agent, built on Anthropic's Claude Agent SDK tool.",
		},
		{
			name: "Main branch rewrites original",
			in:   "Main branch (you will usually use this for PRs)",
			want: "Default branch (you will usually use this for PRs)",
		},
		{
			name: "Codex agent identity rewrites original",
			in:   "You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
			want: "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
		},
		{
			name: "Codex GPT variant rewrites original",
			in:   "You are GPT-5.2 running in the Codex CLI, a terminal-based coding assistant.",
			want: "You are GPT-5.2 running in the Codex CLI tool, a terminal-based coding assistant.",
		},
		{
			name: "DeepSeek harness identity rewrites original",
			in:   "You are an AI agent powered by DeepSeek Harness.",
			want: "You are an AI assistant.",
		},
		{
			name: "DeepSeek coding-agent identity rewrites original",
			in:   "You are a coding agent powered by the deepseek-v4-flash model.",
			want: "You are a coding agent using the deepseek-v4-flash model.",
		},
		{
			name: "DeepSeek harness web GUI attribution rewritten",
			in:   "You interact through the DeepSeek Harness Web GUI.",
			want: "You interact through the web interface.",
		},
		{
			name: "feedback give to provide",
			in:   "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			want: "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		},
		{
			name: "already rewritten idempotent",
			in:   "You are Claude Code, Anthropic's official CLI tool for Claude.",
			want: "You are Claude Code, Anthropic's official CLI tool for Claude.",
		},
		{
			name: "partial template unchanged",
			in:   "    You are Claude Code, but this is not the blocked template",
			want: "    You are Claude Code, but this is not the blocked template",
		},
		{
			name: "ordinary cc assignment",
			in:   "Set cc_mode=display; want: other",
			want: "Set cc_mode=display; want: other",
		},
		{
			name: "unrelated Anthropic URL",
			in:   "prefix https://github.com/anthropics/example suffix",
			want: "prefix https://github.com/anthropics/example suffix",
		},
		{
			name: "no fingerprint",
			in:   "Hello world",
			want: "Hello world",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBlockedTemplates(tt.in); got != tt.want {
				t.Fatalf("sanitizeBlockedTemplates(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPrepareUpstreamBodyRewritesFingerprintPreservingCallerContent(t *testing.T) {
	body := []byte(`{"model":"other-model","reasoning_effort":"low","messages":[{"role":"system","content":"keep me"},{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."}]}`)
	out := prepareUpstreamBody(body, nil, nil, "other-model")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", obj["reasoning_effort"])
	}
	messages := obj["messages"].([]any)
	kept := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(kept, "keep me") {
		t.Fatalf("caller content lost: %q", kept)
	}
	rewritten := messages[1].(map[string]any)["content"].(string)
	if strings.Contains(rewritten, "official CLI for Claude") {
		t.Fatalf("identity fingerprint not rewritten: %q", rewritten)
	}
	if !strings.Contains(rewritten, "official CLI tool for Claude") {
		t.Fatalf("rewrite target missing: %q", rewritten)
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
