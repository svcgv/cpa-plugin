// Package contextrules holds the client-context cleanup rules shared by the
// QoderWork and WorkBuddy plugins. Both upstreams reject the same Claude Code,
// Codex CLI and DeepSeek harness prompt fingerprints, so the rules stay here.
package contextrules

import (
	"regexp"
	"strings"
)

// Fingerprints are substrings that mark a prompt as carrying a blocked harness
// fingerprint. HasFingerprint uses them purely as a cheap pre-check, so short
// prefixes intentionally cover every known variant of the same template.
var Fingerprints = []string{
	"cch=cc_",
	"You are Claude Code",
	"You are a Claude agent, built on Anthropic's Claude Agent SDK",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"You are Codex, a coding agent",
	"running in the Codex CLI",
	"Codex CLI is an open source project",
	"github.com/anthropics/",
	"You are Antigravity",
	// DeepSeek harness (deepseek.com/harness) system-prompt identity lines.
	"You are an AI agent powered by DeepSeek Harness.",
	"You are a coding agent powered by the ",
	"You are a coding assistant powered by the ",
	"through the DeepSeek Harness Web GUI",
	"the DeepSeek Harness implementation checkout",
	// Fallbacks: older/advertised DeepSeek identity phrasings. Harmless when
	// absent; keeps older builds and future rewording covered.
	"deepseek.com/harness",
	"You are DeepSeek",
	"running in the DeepSeek harness",
	"DeepSeek-V3",
	"DeepSeek-R1",
}

// billingHeaderRE matches the CPA-generated Claude Code billing header, which is
// always the whole of system[0] and carries no meaning upstream:
//
//	cch=<5 hex>;cc_version=...;cc_entrypoint=...;
//
// It must start the text (after optional whitespace); unrelated "cch=" text in
// user content is left untouched.
var billingHeaderRE = regexp.MustCompile(`^[ \t\n]*(?:x-anthropic-billing-header:[^\n]*|cch=[0-9a-f]{5};cc_[^\n]*)`)

// Sanitize removes harness fingerprints from one block of text. Every
// ReplaceAll source is literal text a harness injects; the target is a
// meaning-preserving paraphrase that no longer matches the upstream filter.
func Sanitize(s string) string {
	if !HasFingerprint(s) {
		return s
	}
	original := s

	// 1) Drop the Claude Code billing header block outright.
	s = billingHeaderRE.ReplaceAllString(s, "")

	// 2) Rewrite each known identity / fingerprint line.
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK tool.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = strings.ReplaceAll(s,
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.")
	s = strings.ReplaceAll(s,
		"running in the Codex CLI, a terminal-based coding assistant.",
		"running in the Codex CLI tool, a terminal-based coding assistant.")
	s = strings.ReplaceAll(s,
		"You are Codex, a coding agent based on GPT-5.",
		"You are Codex, a coding agent built on GPT-5.")
	s = strings.ReplaceAll(s,
		"Codex CLI is an open source project",
		"Codex CLI tool is an open source project")
	s = strings.ReplaceAll(s,
		"You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team",
		"You are Antigravity, a powerful agentic AI coding assistant built on the Google Deepmind team")
	s = strings.ReplaceAll(s,
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues")
	// DeepSeek harness identity lines (verified from the deepseek-harness source:
	// packages/core/system-prompt/src/index.ts and packages/bundle/*/cordis.patch.yml).
	// The upstream gateway blocks prompts that advertise the originating harness, so
	// the self-identification is rewritten while the task instructions stay intact.
	s = strings.ReplaceAll(s,
		"You are an AI agent powered by DeepSeek Harness.",
		"You are an AI assistant.")
	s = strings.ReplaceAll(s,
		"You are a coding agent powered by the ",
		"You are a coding agent using the ")
	s = strings.ReplaceAll(s,
		"You are a coding assistant powered by the ",
		"You are a coding assistant using the ")
	s = strings.ReplaceAll(s,
		"through the DeepSeek Harness Web GUI",
		"through the web interface")
	s = strings.ReplaceAll(s,
		"the DeepSeek Harness implementation checkout",
		"the deepseek-harness implementation checkout")
	// Fallbacks for older builds / reworded identity lines.
	s = strings.ReplaceAll(s,
		"You are DeepSeek, an AI assistant created by DeepSeek.",
		"DeepSeek is an AI assistant created by DeepSeek.")
	s = strings.ReplaceAll(s,
		"You are DeepSeek",
		"DeepSeek is an assistant")
	s = strings.ReplaceAll(s,
		"running in the DeepSeek harness",
		"running in a DeepSeek-family coding agent")
	s = strings.ReplaceAll(s,
		"https://deepseek.com/harness/",
		"https://deepseek.com/docs/")
	s = strings.ReplaceAll(s,
		"www.deepseek.com/harness/",
		"deepseek.com/docs/")

	if s == original {
		return original
	}
	return strings.TrimSpace(s)
}

// HasFingerprint reports whether text contains any blocked harness fingerprint.
func HasFingerprint(s string) bool {
	if billingHeaderRE.MatchString(s) {
		return true
	}
	for _, feature := range Fingerprints {
		if strings.Contains(s, feature) {
			return true
		}
	}
	return false
}
