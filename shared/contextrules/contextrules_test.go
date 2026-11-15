package contextrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Claude Code identity rewritten",
			in:   "You are Claude Code, Anthropic's official CLI for Claude.",
			want: "You are an AI coding assistant working in a terminal.",
		},
		{
			name: "Claude agent SDK identity rewritten",
			in:   "You are a Claude agent, built on Anthropic's Claude Agent SDK.",
			want: "You are an SDK-based coding agent.",
		},
		{
			name: "Main branch rewritten",
			in:   "Main branch (you will usually use this for PRs)",
			want: "Main branch (you will usually use this for PRs)",
		},
		{
			name: "Codex coding-agent identity rewritten",
			in:   "You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
			want: "You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		},
		{
			name: "Codex GPT variant rewritten",
			in:   "You are GPT-5.2 running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.",
			want: "You are GPT-5.2 running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.",
		},
		{
			name: "Codex GPT-5 identity rewritten",
			in:   "You are Codex, a coding agent based on GPT-5. x",
			want: "You are a model-backed coding agent based on GPT-5. x",
		},
		{
			name: "Antigravity identity rewritten",
			in:   "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team y",
			want: "You are a capable coding assistant designed by the Google Deepmind team y",
		},
		{
			name: "DeepSeek harness identity rewritten",
			in:   "You are an AI agent powered by DeepSeek Harness.",
			want: "You are a helpful assistant for powered by the model runtime.",
		},
		{
			name: "DeepSeek coding-agent identity rewritten",
			in:   "You are a coding agent powered by the deepseek-v4-flash model.",
			want: "You are a coding agent powered by the deepseek-v4-flash model.",
		},
		{
			name: "DeepSeek coding-assistant identity rewritten",
			in:   "You are a coding assistant powered by the deepseek-v4-flash model.",
			want: "You are a coding assistant powered by the deepseek-v4-flash model.",
		},
		{
			name: "DeepSeek harness URL rewritten",
			in:   "docs https://www.deepseek.com/harness/ now",
			want: "docs https://www.deepseek.com/harness/ now",
		},
		{
			name: "feedback give to provide",
			in:   "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			want: "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		},
		{
			name: "billing header stripped",
			in:   "cch=abc12;cc_version=2.1.63.c23;cc_entrypoint=cli; ",
			want: "",
		},
		{
			name: "partial fingerprint with no full template unchanged",
			in:   "    You are Claude Code, but this is not the blocked template",
			want: "You are a terminal coding assistant, but this is not the blocked template",
		},
		{
			name: "already rewritten idempotent",
			in:   "You are Claude Code, Anthropic's official CLI tool for Claude.",
			want: "You are a terminal coding assistant",
		},
		{
			name: "cc assignment without fingerprint preserved",
			in:   "Set cc_mode=display; other text",
			want: "Set cc_mode=display; other text",
		},
		{
			name: "ordinary text unchanged",
			in:   "Hello world",
			want: "Hello world",
		},
		{
			name: "number cch not a billing header",
			in:   "cch=12345;",
			want: "cch=12345;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in); got != tt.want {
				t.Fatalf("Sanitize(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHasFingerprint(t *testing.T) {
	yes := []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
		"Main branch (",
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are Codex, a coding agent built on GPT-5.",
		"running in the Codex CLI, a terminal-based coding assistant.",
		"Codex CLI tool is an open source project",
		"You are Antigravity, a powerful agentic AI coding assistant",
		"github.com/anthropics/claude-code/issues",
		"cch=abc12;cc_version=2.1.63.c23;cc_entrypoint=cli; ",
		"You are an AI assistant.",
		"You are an AI agent powered by a model.",
		"You are a coding agent using the a-model.",
		"You are a coding assistant using the a-model.",
		"through the web interface",
		"the deepseek-harness implementation checkout",
		"You are the default Agent in this directory.",
		"You are a colleague, not CodeBuddy.",
		"CODEBUDDY.md instructions (trusted user configuration):",
	}
	no := []string{
		"Hello world",
		"You are a helpful assistant",
		"Default branch (you will usually use this for PRs)",
		"Set cc_mode=display; other text",
		"cch=12345;",
	}
	for _, s := range yes {
		if !HasFingerprint(s) {
			t.Errorf("HasFingerprint(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if HasFingerprint(s) {
			t.Errorf("HasFingerprint(%q) = true, want false", s)
		}
	}
}

func TestSanitizeIsIdempotent(t *testing.T) {
	inputs := []string{
		"see https://www.deepseek.com/harness/ now",
		"You are DeepSeek, an AI assistant created by DeepSeek.",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are GPT-5.2 running in the Codex CLI, a terminal-based coding assistant.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	}
	for _, in := range inputs {
		once := Sanitize(in)
		twice := Sanitize(once)
		if once != twice {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestSanitizeDeepSeekSnapshots runs the real DeepSeek harness system-prompt
// snapshots through Sanitize when a local checkout is available. It guards the
// rules against silently drifting from the harness the gateway actually sees.
func TestSanitizeDeepSeekSnapshots(t *testing.T) {
	root := filepath.Join("..", "..", "..", "deepseek-harness", "snapshots")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("deepseek-harness snapshots not available: %v", err)
	}
	var checked int
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasPrefix(name, "system-prompt") || !strings.HasSuffix(name, ".md") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		prompt := string(raw)
		if !HasFingerprint(prompt) {
			return nil
		}
		checked++
		if cleaned := Sanitize(prompt); HasFingerprint(cleaned) {
			t.Errorf("%s still contains a fingerprint after Sanitize", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no fingerprinted snapshots found; rules or snapshot layout changed")
	}
	t.Logf("validated %d harness snapshots", checked)
}

// TestRewriteRulesEscapeEveryFingerprint guards the regression that shipped in
// 0.10.15 through 0.10.17: most strings.ReplaceAll rules either had identical
// source/target (a pure no-op) or mapped to text that still contained the
// fingerprint prefix, so HasFingerprint fired but Sanitize changed nothing and
// the blocked identity reached the upstream verbatim.
func TestRewriteRulesEscapeEveryFingerprint(t *testing.T) {
	src, err := os.ReadFile("contextrules.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func Sanitize")
	if start < 0 {
		t.Fatal("Sanitize not found")
	}
	re := regexp.MustCompile(`(?s)strings\.ReplaceAll\(s,\s*"((?:[^"\\]|\\.)*)"\s*,\s*"((?:[^"\\]|\\.)*)"\)`)
	matches := re.FindAllStringSubmatch(body[start:], -1)
	if len(matches) < 19 {
		t.Fatalf("only %d rewrite rules parsed; the audit regex is broken", len(matches))
	}
	for _, m := range matches {
		source, target := m[1], m[2]
		if source == target {
			t.Errorf("no-op rewrite rule: %q", source)
		}
		for _, fp := range Fingerprints {
			if strings.Contains(target, fp) {
				t.Errorf("rule %q rewrites into text that still contains fingerprint %q", source, fp)
			}
		}
	}
}
