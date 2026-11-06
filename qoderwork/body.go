// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
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
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`

	// Images holds image URLs found in this message's content parts. The
	// upstream agent body accepts them via image_urls /
	// chat_context.imageUrls, so dropping them makes an image-only request
	// fail with "no user message in request".
	Images []string `json:"-"`
}

// UnmarshalJSON accepts a string or an array of content parts. Parts may carry
// "text" (input_text/output_text/text) or "input_text"/"output_text" variants;
// non-text parts (images, tool results) are ignored, since the upstream agent
// body only takes a prompt string.
func (m *openAIMessage) UnmarshalJSON(raw []byte) error {
	var probe struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		Name             string          `json:"name"`
		ReasoningContent string          `json:"reasoning_content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	m.Role = probe.Role
	m.Content, m.Images = contentParts(probe.Content)
	m.ToolCalls, m.ToolCallID, m.Name, m.ReasoningContent = probe.ToolCalls, probe.ToolCallID, probe.Name, probe.ReasoningContent
	return nil
}

// contentText flattens an OpenAI content value into plain text.
func contentParts(raw json.RawMessage) (string, []string) {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || string(v) == "null" {
		return "", nil
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s, nil
		}
		return "", nil
	}
	if v[0] != '[' {
		return "", nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(v, &parts) != nil {
		return "", nil
	}
	var out []string
	var images []string
	for _, part := range parts {
		var p struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Content  json.RawMessage `json:"content"`
			Input    string          `json:"input_text"`
			Output   string          `json:"output_text"`
			ImageURL json.RawMessage `json:"image_url"`
			FileData json.RawMessage `json:"file_data"`
		}
		if json.Unmarshal(part, &p) != nil {
			continue
		}
		if url := imageURLFromPart(p.Type, p.ImageURL, p.FileData); url != "" {
			images = append(images, url)
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
			if nested, nestedImages := contentParts(p.Content); nested != "" {
				out = append(out, nested)
				images = append(images, nestedImages...)
			}
		}
	}
	return strings.Join(out, "\n"), images
}

// imageURLFromPart extracts an image URL from a content part. Accepts the
// Responses API shapes: {"type":"input_image","image_url":"https://..."},
// {"type":"input_image","image_url":{"url":"https://..."}}, plus
// {"type":"image_url","url":"..."} and inline base64 file_data.
func imageURLFromPart(partType string, imageURL, fileData json.RawMessage) string {
	t := strings.ToLower(strings.TrimSpace(partType))
	if t != "input_image" && t != "image_url" && t != "image" {
		return ""
	}
	for _, raw := range []json.RawMessage{imageURL, fileData} {
		v := bytes.TrimSpace(raw)
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		if v[0] == '"' {
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			continue
		}
		if v[0] == '{' {
			var nested struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(v, &nested) == nil && strings.TrimSpace(nested.URL) != "" {
				return strings.TrimSpace(nested.URL)
			}
		}
	}
	return ""
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	Stream              bool            `json:"stream"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	MaxOutputTokens     *int            `json:"max_output_tokens"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	ClientSessionID     string          `json:"-"`

	// Input carries the Responses API form when the caller (or CPA) forwards
	// it directly: "input" may be a string or an array of typed items.
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`

	// Clients sometimes disclose their real working directory. Codex puts it
	// in session metadata; some OpenAI-compatible clients send it at the top
	// level. The plugin runs inside CPA and cannot read it any other way, so
	// use it when present instead of claiming a directory that is not the
	// user's project.
	CWD      string         `json:"cwd"`
	Metadata map[string]any `json:"metadata"`
}

// effectiveMessages returns the conversation, accepting either the Chat
// Completions "messages" field or the Responses API "input" field.
func (r *openAIRequest) effectiveMessages() []openAIMessage {
	if len(r.Messages) > 0 {
		return r.Messages
	}
	return parseResponsesInput(r.Input)
}

// parseResponsesInput converts a Responses API "input" value into messages.
// Accepts a plain string, an array of message objects, and an array of typed
// content items (which become one user message).
func parseResponsesInput(raw json.RawMessage) []openAIMessage {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
			return []openAIMessage{{Role: "user", Content: s}}
		}
		return nil
	}
	if v[0] != '[' {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(v, &items) != nil {
		return nil
	}
	// Responses API represents function calls and outputs as top-level items.
	var msgs []openAIMessage
	asMessages := true
	for _, item := range items {
		var typed struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		_ = json.Unmarshal(item, &typed)
		switch typed.Type {
		case "function_call":
			calls, _ := json.Marshal([]any{map[string]any{"id": typed.CallID, "type": "function", "function": map[string]any{"name": typed.Name, "arguments": typed.Arguments}}})
			msgs = append(msgs, openAIMessage{Role: "assistant", ToolCalls: calls})
			continue
		case "function_call_output":
			content, images := contentParts(typed.Output)
			msgs = append(msgs, openAIMessage{Role: "tool", ToolCallID: typed.CallID, Content: content, Images: images})
			continue
		}
		var m openAIMessage
		if json.Unmarshal(item, &m) != nil || m.Role == "" {
			asMessages = false
			break
		}
		msgs = append(msgs, m)
	}
	if asMessages && len(msgs) > 0 {
		return msgs
	}
	// Array of content parts: flatten into a single user message.
	text, images := contentParts(v)
	if text == "" && len(images) == 0 {
		return nil
	}
	return []openAIMessage{{Role: "user", Content: text, Images: images}}
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

// latestUserImages returns the images attached to the last user message, so an
// image-only request is still delivered to the upstream.
func latestUserImages(messages []openAIMessage) []string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if len(messages[i].Images) > 0 {
			return messages[i].Images
		}
	}
	return nil
}

// messageContent renders one message for the upstream body. Text-only messages
// stay plain strings; messages carrying images become a content-part array so
// the attachment reaches the model. The shape mirrors the Qoder CLI runtime:
//
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}
func messageContent(m openAIMessage) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]any, 0, len(m.Images)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, u := range m.Images {
		if strings.TrimSpace(u) == "" {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": u},
		})
	}
	if len(parts) == 0 {
		return m.Content
	}
	return parts
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string, sa *storedAuth) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	messages := append([]openAIMessage(nil), req.effectiveMessages()...)
	if req.Instructions != "" {
		messages = append([]openAIMessage{{Role: "system", Content: req.Instructions}}, messages...)
	}
	reasoningEffort, reasoningEnabled, err := resolveReasoning(sa, modelKey, req.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	if sa != nil {
		for i := range messages {
			if len(messages[i].Images) == 0 {
				continue
			}
			if meta, ok := metadataForModel(sa, modelKey); !ok || !meta.vision() {
				return nil, fmt.Errorf("模型 %s 未声明支持图片输入", modelKey)
			}
			messages[i].Images = append([]string(nil), messages[i].Images...)
			for j, image := range messages[i].Images {
				messages[i].Images[j] = publishQoderImage(sa, image)
			}
		}
	}
	prompt := extractLatestUserPrompt(messages)
	images := latestUserImages(messages)
	if prompt == "" && len(images) == 0 && len(messages) == 0 {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	requestSetID := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = requestSetID
	base["session_id"] = referenceSessionID(sa, modelKey, req.ClientSessionID)
	base["stream"] = true
	base["aliyun_user_type"] = ""
	base["system"] = ""
	base["session_type"] = "qoderclicn"
	if sa != nil && sa.region() == regionIntl {
		base["session_type"] = "qodercli"
	}
	base["agent_id"] = "agent_common"

	// model_config
	base["model_config"] = map[string]any{"key": modelKey, "is_reasoning": false, "source": "system"}
	applyModelMetadata(base, sa, modelKey)
	if mc, ok := base["model_config"].(map[string]any); ok && reasoningEnabled != nil {
		mc["is_reasoning"] = *reasoningEnabled
	}
	maxTokens := req.MaxTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = req.MaxCompletionTokens
	}
	if req.MaxOutputTokens != nil {
		maxTokens = req.MaxOutputTokens
	}
	if meta, ok := metadataForModel(sa, modelKey); ok {
		if declared := meta.maxCompletion(); declared > 0 && (maxTokens == nil || int64(*maxTokens) > declared) {
			limit := int(declared)
			maxTokens = &limit
		}
	}
	parameters := map[string]any{}
	if maxTokens != nil && *maxTokens > 0 {
		parameters["max_tokens"] = *maxTokens
		if mc, ok := base["model_config"].(map[string]any); ok {
			mc["max_output_tokens"] = *maxTokens
		}
	}
	if reasoningEffort != "" {
		parameters["reasoning_effort"] = reasoningEffort
	}
	base["parameters"] = parameters
	base["tools"] = []any{}
	if len(req.Tools) > 0 {
		var tools any
		if json.Unmarshal(req.Tools, &tools) == nil {
			base["tools"] = tools
		}
	}
	if len(req.ToolChoice) > 0 {
		var choice any
		if json.Unmarshal(req.ToolChoice, &choice) == nil {
			base["tool_choice"] = choice
		}
	}

	modelConfig := base["model_config"].(map[string]any)
	base["chat_context"] = map[string]any{"chatPrompt": "", "imageUrls": nil, "extra": map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": modelKey, "is_reasoning": modelConfig["is_reasoning"]}, "originalContent": prompt}, "features": []any{}, "text": prompt}

	// Images travel inline in messages, as OpenAI-style content parts. That is
	// exactly what the Qoder CLI runtime does (its message builder emits
	// {type:"image_url", image_url:{url:"data:<mime>;base64,..."}}), and a live
	// gateway probe confirms the model sees them there. chat_context.imageUrls
	// stays null: filling it (with either a client URL or a re-uploaded one)
	// never delivered the image to the model.

	var out []any
	client := clientSystemText(messages)
	if client != "" {
		out = append(out, map[string]any{"role": "system", "content": client})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue // already emitted first
		}
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		item := map[string]any{"role": role, "content": messageContent(m)}
		if len(m.ToolCalls) > 0 {
			var calls any
			if json.Unmarshal(m.ToolCalls, &calls) == nil {
				item["tool_calls"] = calls
			}
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			item["name"] = m.Name
		}
		if m.ReasoningContent != "" {
			item["reasoning_content"] = m.ReasoningContent
		}
		out = append(out, item)
	}
	base["messages"] = out

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["version"] = "1.0.0"
		biz["id"] = requestSetID
		biz["begin_at"] = time.Now().UnixMilli()
		if runes := []rune(prompt); len(runes) > 30 {
			biz["name"] = string(runes[:30])
		} else {
			biz["name"] = prompt
		}
	}

	return json.Marshal(base)
}

func referenceSessionID(sa *storedAuth, modelKey, clientSession string) string {
	uid := ""
	if sa != nil {
		uid = sa.Account.UID
	}
	sum := sha256.Sum256([]byte("qoder-session\x00" + uid + "\x00" + modelKey))
	if strings.TrimSpace(clientSession) == "" {
		clientSession = uuid.NewString()
	}
	return fmt.Sprintf("%x-%s", sum[:8], clientSession)
}

func requestClientSession(headers http.Header, hostMetadata, payloadMetadata map[string]any) string {
	for _, value := range []string{headers.Get("X-Session-Id"), headers.Get("Session-Id"), headers.Get("Session_Id"), metaString(hostMetadata, "session_id"), metaString(payloadMetadata, "session_id")} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
