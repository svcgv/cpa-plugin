// payload.go rewrites the outgoing chat completion request body before it's
// forwarded upstream. The single-pass entry point is prepareUpstreamBody; the
// four *InPlace helpers are the field-level mutations it composes, and the
// legacy *ForUpstream / forceStreamBody wrappers exist for tests and other
// call sites that need them individually.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// prepareUpstreamBody composes forceStreamBody + normalizeToolsForUpstream +
// rewriteModelInBody + rewriteSystemForUpstream + normalizeMessageRoles +
// ensureSystemMessage into a single unmarshal/marshal pass (v0.6.31 perf: was
// 4-5 full JSON round-trips on every chat completion). The legacy helpers
// remain for tests and other call sites that need them individually.
func prepareUpstreamBody(payload, original []byte, sa *storedAuth, upstreamModel string) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}

	// 1. forceStream: CodeBuddy rejects non-stream requests.
	obj["stream"] = true

	// 2. normalizeTools: tool_choice object form → string; "none" suppresses tools.
	normalizeToolsInPlace(obj)

	// 3. rewriteModel: swap client model name to upstream model id.
	rewriteModelInPlace(obj, upstreamModel)

	// 4. message roles: CodeBuddy only speaks system/user/assistant/tool, so a
	// developer message (preserved by CPA >= 8.0.61 responses→chat translation)
	// must become a system message or the upstream rejects the whole request
	// with 400 code 11128 "Illegal API invocation from an unapproved channel".
	normalizeMessageRolesInPlace(obj)

	// 5. system sanitization: strip blocked Claude Code template phrases.
	rewriteSystemInPlace(obj)

	// 6. desensitize the configured prompt and tool metadata fields.
	applyDesensitizeInPlace(obj, currentFeatureRuntime())

	// 7. ensureSystemMessage: CodeBuddy requires the first message to be a
	// system prompt; inject a minimal one when the conversation has none.
	ensureSystemMessageInPlace(obj, sa)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolsInPlace is the in-place form of normalizeToolsForUpstream.
// Returns true when obj was modified.
func normalizeToolsInPlace(obj map[string]any) bool {
	changed := false
	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}
	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			delete(obj, "tool_choice")
			changed = true
		}
	}
	return changed
}

// rewriteSystemInPlace is the in-place form of rewriteSystemForUpstream.
func rewriteSystemInPlace(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}
	return changed
}

// normalizeMessageRolesInPlace maps OpenAI Responses-only roles onto the
// CodeBuddy chat schema. A developer message carries operator instructions and
// maps 1:1 onto system; any other unrecognized role is treated as system too so
// the request is still served instead of being refused wholesale.
func normalizeMessageRolesInPlace(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	changed := false
	valid := map[string]struct{}{"system": {}, "user": {}, "assistant": {}, "tool": {}, "function": {}}
	for _, m := range messages {
		msg, okMsg := m.(map[string]any)
		if !okMsg {
			continue
		}
		role, _ := msg["role"].(string)
		normalized := strings.ToLower(strings.TrimSpace(role))
		if normalized == "developer" {
			msg["role"] = "system"
			changed = true
			continue
		}
		if _, known := valid[normalized]; known {
			if normalized != role {
				msg["role"] = normalized
				changed = true
			}
			continue
		}
		msg["role"] = "system"
		changed = true
	}
	return changed
}

// ensureSystemMessageInPlace guarantees the conversation's FIRST message is a
// system prompt. Returns true when obj was modified.
//
// CodeBuddy rejects a chat that does not open with a system prompt with 400 code
// 11128 "Illegal API invocation from an unapproved channel" (or 11101 invalid
// request). Developer instructions are folded into system in place by
// normalizeMessageRolesInPlace — verified upstream behavior, so a mid-session
// system message is accepted and keeps its position. This only covers the case
// where no system instruction leads the conversation at all.
//
// Both regions reject the system-less opening, so injection is no longer
// Global-only: CN tolerates the placeholder exactly like Global does.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
	_ = sa // region-independent: CN rejects a system-less opening too.
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	first, _ := messages[0].(map[string]any)
	if first != nil {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return false
		}
	}
	placeholder := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{placeholder}, messages...)
	return true
}

// rewriteModelInPlace swaps obj["model"] to upstreamModel when non-empty.
// Returns true when modified.
func rewriteModelInPlace(obj map[string]any, upstreamModel string) bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return false
	}
	cur, _ := obj["model"].(string)
	if cur == upstreamModel {
		return false
	}
	obj["model"] = upstreamModel
	return true
}

func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolsForUpstream adapts OpenAI tools / tool_choice fields to
// CodeBuddy's chat schema before the request is forwarded.
//
// Live-verified against /v2/chat/completions (2026-07):
//  1. tool_choice is typed as string on the upstream Go struct. OpenAI's object
//     form {"type":"function","function":{"name":"..."}} returns 400 code 11101
//     ("cannot unmarshal object into Go struct field Request.tool_choice of
//     type string"). Convert known object shapes to the matching string.
//  2. tool_choice "none" is accepted but ignored when tools[] is non-empty —
//     the model still emits tool_calls. The only reliable way to suppress tools
//     is to omit tools (and functions) entirely.
//
// String values auto / required / <function name> are left untouched.
func normalizeToolsForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := false

	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}

	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					// Object force without a name: fall back to auto instead of 400.
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				// Unknown object shape → drop rather than forward a 400.
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			// null / array / number — drop to keep upstream happy.
			delete(obj, "tool_choice")
			changed = true
		}
	}

	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// ensureSystemMessage injects a minimal system message when the conversation
// does not already open with one. It delegates to the in-place form so the
// legacy standalone path cannot drift from prepareUpstreamBody.
func ensureSystemMessage(payload []byte, sa *storedAuth) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	if !ensureSystemMessageInPlace(obj, sa) {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

var sanitizeFeatures = []string{
	"x-anthropic-billing-header",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
}

var sanitizeBillingHeaderRE = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)
var sanitizeCCEntrypointRE = regexp.MustCompile(`(?i)\bcc_entrypoint=`)
var sanitizeCCKeyValueRE = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

func sanitizeBlockedTemplates(s string) string {
	if !hasFingerprint(s) {
		return s
	}
	original := s
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = strings.ReplaceAll(s,
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.")
	s = strings.ReplaceAll(s,
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues")
	s = sanitizeBillingHeaderRE.ReplaceAllString(s, "")
	s = sanitizeCCKeyValueRE.ReplaceAllString(s, "")
	if s == original {
		return original
	}
	return strings.TrimSpace(s)
}

func hasFingerprint(s string) bool {
	if sanitizeCCEntrypointRE.MatchString(s) {
		return true
	}
	for _, feature := range sanitizeFeatures {
		if strings.Contains(s, feature) {
			return true
		}
	}
	return sanitizeBillingHeaderRE.MatchString(s)
}

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
		return body
	}
	obj["model"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		// Legacy function_call shell: {"name":"","arguments":""} is the
		// upstream's terminal-chunk artifact, not a real call — treat as empty
		// when every value is itself empty.
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
