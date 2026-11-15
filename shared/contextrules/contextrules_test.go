package contextrules

import "testing"

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Claude Code identity",
			in:   "You are Claude Code, Anthropic's official CLI tool for Claude.",
			want: "You are Claude Code, Anthropic's official CLI tool for Claude.",
		},
		{
			name: "default branch",
			in:   "Default branch (you will usually use this for PRs)",
			want: "Default branch (you will usually use this for PRs)",
		},
		{
			name: "Codex CLI identity",
			in:   "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
			want: "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
		},
		{
			name: "feedback URL",
			in:   "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			want: "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		},
		{
			name: "partial template text remains unchanged",
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
			in:   "prefix suffix",
			want: "prefix suffix",
		},
		{
			name: "no fingerprint",
			in:   "Hello world",
			want: "Hello world",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in); got != tt.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
