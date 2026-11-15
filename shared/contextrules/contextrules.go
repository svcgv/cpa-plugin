// Package contextrules holds the client-context cleanup rules shared by the
// QoderWork and WorkBuddy plugins. Both upstreams reject the same Claude Code
// and Codex CLI prompt fingerprints, so the rule set must stay in one place.
package contextrules

import (
	"regexp"
	"strings"
)

// Fingerprints 是上游会拦截的系统提示特征。
var Fingerprints = []string{
	"x-anthropic-billing-header",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
}

var billingHeaderRE = regexp.MustCompile(`(?i)\n]*;?\s*`)
var ccEntrypointRE = regexp.MustCompile(`(?i)\bcc_entrypoint=`)
var ccKeyValueRE = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// Sanitize 净化单段文本中被上游网关拦截的提示指纹。
func Sanitize(s string) string {
	if !HasFingerprint(s) {
		return s
	}
	original := s
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Default branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = strings.ReplaceAll(s,
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.")
	s = strings.ReplaceAll(s,
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues")
	s = billingHeaderRE.ReplaceAllString(s, "")
	s = ccKeyValueRE.ReplaceAllString(s, "")
	if s == original {
		return original
	}
	return strings.TrimSpace(s)
}

// HasFingerprint 判断文本是否包含任一被拦截的提示指纹。
func HasFingerprint(s string) bool {
	if ccEntrypointRE.MatchString(s) {
		return true
	}
	for _, feature := range Fingerprints {
		if strings.Contains(s, feature) {
			return true
		}
	}
	return billingHeaderRE.MatchString(s)
}
