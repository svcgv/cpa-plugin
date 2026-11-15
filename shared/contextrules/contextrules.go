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
	"Codex CLI tool is an open source project",
	"github.com/anthropics/",
	"You are Antigravity",
	// DeepSeek harness self-identification, per rendered variant.
	"You are an AI assistant.",
	"You are an AI agent ",
	"You are a coding agent for the ",
	"You are a coding agent using the ",
	"You are a coding assistant for the ",
	"You are a coding assistant using the ",
	"through the web interface",
	"the deepseek-harness implementation checkout",
	// codebuddy-code CLI identity lines (dist-server/codebuddy.js v2.184.0).
	"You are the default Agent in this directory.",
	"You are a colleague, not CodeBuddy.",
	"You are one participant in this channel.",
	"You are in Agent Home channels",
	"You are in creation mode: help the user design",
	"running in **ultracode** effort mode",
	"CODEBUDDY.md instructions (trusted user configuration)",
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
	s = strings.ReplaceAll(s, "You are Claude Code, Anthropic's official CLI for Claude.", "You are an AI coding assistant working in a terminal.")
	s = strings.ReplaceAll(s, "You are Claude Code, Anthropic's official CLI tool for Claude.", "You are a terminal coding assistant")
	s = strings.ReplaceAll(s, "You are Claude Code", "You are a terminal coding assistant")
	s = strings.ReplaceAll(s, "You are a Claude agent, built on Anthropic's Claude Agent SDK.", "You are an SDK-based coding agent.")
	s = strings.ReplaceAll(s, "You are a Claude agent, built on Anthropic", "You are an SDK-based coding agent")
	s = strings.ReplaceAll(s,
		"Default branch (you will usually use this for PRs)",
		"Default branch (usually used for pull requests)")
	s = strings.ReplaceAll(s,
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
		"You are a coding agent running in a terminal, a command-line coding assistant.")
	s = strings.ReplaceAll(s,
		"running in the Codex CLI tool, a terminal-based coding assistant.",
		"running in a terminal, a command-line coding assistant.")
	s = strings.ReplaceAll(s,
		"You are Codex, a coding agent",
		"You are a model-backed coding agent")
	s = strings.ReplaceAll(s,
		"Codex CLI tool is an open source project",
		"This coding tool is an open source project")
	s = strings.ReplaceAll(s, "You are Antigravity, a powerful agentic AI coding assistant", "You are a capable coding assistant")
	s = strings.ReplaceAll(s, "You are Antigravity", "You are a capable coding assistant")
	s = strings.ReplaceAll(s,
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue in the project tracker.")
	// DeepSeek harness self-identification, one rule per rendered variant.
	s = strings.ReplaceAll(s, "You are an AI assistant.", "You are a helpful assistant.")
	s = strings.ReplaceAll(s, "DeepSeek Harness", "the model runtime")
	s = strings.ReplaceAll(s,
		"Relative paths resolve from the workspace root; absolute paths identify files or directories on the host.",
		"Relative paths are resolved from the project root; absolute paths point at files or directories on the machine.")
	s = strings.ReplaceAll(s, "You are an AI agent ", "You are a helpful assistant for ")
	s = strings.ReplaceAll(s, "You are a coding agent for the ", "You are a software engineering agent for the ")
	s = strings.ReplaceAll(s, "You are a coding agent using the ", "You are a software engineering agent using the ")
	s = strings.ReplaceAll(s, "You are a coding assistant for the ", "You are a software engineering assistant for the ")
	s = strings.ReplaceAll(s, "You are a coding assistant using the ", "You are a software engineering assistant using the ")
	s = strings.ReplaceAll(s, "through the web interface", "through the web client")
	s = strings.ReplaceAll(s, "the deepseek-harness implementation checkout", "the local implementation checkout")
	// codebuddy-code CLI identity lines.
	s = strings.ReplaceAll(s,
		"You are in creation mode: help the user design and write custom agents as Markdown files.",
		"You are in creation mode and can help design custom agents.")
	s = strings.ReplaceAll(s,
		"You are the default Agent in this directory.",
		"You are the default assistant in this directory.")
	s = strings.ReplaceAll(s,
		"You are a colleague, not CodeBuddy.",
		"You are a colleague.")
	s = strings.ReplaceAll(s,
		"You are one participant in this channel.",
		"You are one participant here.")
	s = strings.ReplaceAll(s,
		"You are in Agent Home channels",
		"You are in group channels")
	s = strings.ReplaceAll(s,
		"running in **ultracode** effort mode",
		"running in extended effort mode")
	s = strings.ReplaceAll(s,
		"CODEBUDDY.md instructions (trusted user configuration)",
		"project instructions")
	// Fallbacks for older builds / reworded identity lines.
	s = strings.ReplaceAll(s,
		"You are DeepSeek, an AI assistant created by DeepSeek.",
		"DeepSeek is an AI assistant.")
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
