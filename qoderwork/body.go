// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
// Unknown names pass through unchanged (server silently routes to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "qoder-auto", "auto":
		return "auto"
	case "qwen3.8-max-preview", "qwen3.8-max", "qmodel_preview":
		return "qmodel_preview"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	case "glm-5.2", "gm51model":
		return "gm51model"
	case "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format.
//
// Content is either a plain string or a list of typed parts. Codex (and the
// OpenAI Responses API) send:
//
//	"content": [{"type":"input_text","text":"hi"}, ...]
//
// while classic Chat Completions send "content": "hi". Both must parse.
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// UnmarshalJSON accepts a string or an array of content parts. Parts may carry
// "text" (input_text/output_text/text) or "input_text"/"output_text" variants;
// non-text parts (images, tool results) are ignored, since the upstream agent
// body only takes a prompt string.
func (m *openAIMessage) UnmarshalJSON(raw []byte) error {
	var probe struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	m.Role = probe.Role
	m.Content = contentText(probe.Content)
	return nil
}

// contentText flattens an OpenAI content value into plain text.
func contentText(raw json.RawMessage) string {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || string(v) == "null" {
		return ""
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	if v[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(v, &parts) != nil {
		return ""
	}
	var out []string
	for _, part := range parts {
		var p struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Content json.RawMessage `json:"content"`
			Input   string          `json:"input_text"`
			Output  string          `json:"output_text"`
		}
		if json.Unmarshal(part, &p) != nil {
			continue
		}
		switch {
		case p.Text != "":
			out = append(out, p.Text)
		case p.Input != "":
			out = append(out, p.Input)
		case p.Output != "":
			out = append(out, p.Output)
		case len(p.Content) > 0:
			// Nested {"type":"message","content":[...]} or {"content":"..."}
			if nested := contentText(p.Content); nested != "" {
				out = append(out, nested)
			}
		}
	}
	return strings.Join(out, "\n")
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`

	// Clients sometimes disclose their real working directory. Codex puts it
	// in session metadata; some OpenAI-compatible clients send it at the top
	// level. The plugin runs inside CPA and cannot read it any other way, so
	// use it when present instead of claiming a directory that is not the
	// user's project.
	CWD      string         `json:"cwd"`
	Metadata map[string]any `json:"metadata"`
}

// clientWorkingDir returns the caller's working directory when the request
// discloses one. Empty means unknown.
func (r *openAIRequest) clientWorkingDir() string {
	for _, v := range []string{r.CWD, metaString(r.Metadata, "cwd"), metaString(r.Metadata, "working_directory"), metaString(r.Metadata, "workingDirectory")} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// extractLatestUserPrompt returns the content of the last user message.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// templateEnvRE matches the <env> block baked into baseprompt.json. That block
// was captured from someone else's machine (D:/Projects/Qoder on Windows with
// an empty date), so it must never reach an upstream request verbatim.
var templateEnvRE = regexp.MustCompile(`(?s)<env>.*?</env>`)

// applyEnvironment rewrites the template's <env> block.
//
// The plugin runs inside the CPA host process and CPA does not forward the
// client's working directory in ExecutorRequest, so os.Getwd() would report the
// *host* directory (e.g. /CLIProxyAPI inside the container) and the model would
// confidently claim to be working in someone else's project. Instead of
// inventing a path, state that it is unknown and let the conversation supply
// real paths.
func applyEnvironment(systemPrompt, clientCWD string) string {
	wd := strings.TrimSpace(clientCWD)
	isRepo := "unknown"
	if wd == "" {
		wd = "unknown (not provided by the client; use paths from the conversation)"
	} else {
		isRepo = "No"
		// The path is on the client's machine, so .git may not be reachable
		// from this process; only assert "Yes" when it is.
		if _, err := os.Stat(filepath.Join(wd, ".git")); err == nil {
			isRepo = "Yes"
		}
	}
	block := fmt.Sprintf("<env>\nWorking directory: %s\nIs directory a git repo: %s\nPlatform: %s\nOS Version: %s\nToday's date: %s\n</env>",
		wd,
		isRepo,
		runtime.GOOS,
		buildMachineOS(),
		time.Now().Format("2006-01-02"),
	)
	if templateEnvRE.MatchString(systemPrompt) {
		return templateEnvRE.ReplaceAllLiteralString(systemPrompt, block)
	}
	return systemPrompt
}

// clientSystemText concatenates the client's own system messages.
func clientSystemText(messages []openAIMessage) string {
	var parts []string
	for _, m := range messages {
		if m.Role == "system" && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// messages: the Qoder template system prompt is kept (the gateway expects
	// that shape) but its <env> block is rewritten for this host. When the
	// client ships its own agent prompt (Codex), it is placed FIRST so its
	// identity and instructions are not overridden by "You are Qoder".
	var out []any
	client := clientSystemText(req.Messages)
	if client != "" {
		// Plan B: the client already ships a full agent prompt (Codex). Drop
		// the Qoder template system prompt entirely so its identity, env and
		// tool names cannot override the client's.
		out = append(out, map[string]any{"role": "system", "content": client})
	} else if msgs, ok := base["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := mm["role"].(string); role == "system" {
				if txt, _ := mm["content"].(string); txt != "" {
					mm["content"] = applyEnvironment(txt, req.clientWorkingDir())
				}
				out = append(out, mm)
			}
		}
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue // already emitted first
		}
		out = append(out, map[string]any{"role": m.Role, "content": m.Content})
	}
	base["messages"] = out

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	return json.Marshal(base)
}
