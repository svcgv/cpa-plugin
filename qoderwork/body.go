// body.go constructs the Qoder CLI agent_chat_generation request body from
// OpenAI-style chat completion inputs.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Sliverkiss/cpa-plugin/shared/contextrules"
	"github.com/google/uuid"
)

const qoderCLIWireVersion = "1.1.89"

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
	ReasoningSig     string          `json:"reasoning_content_signature,omitempty"`
	ToolError        bool            `json:"is_error,omitempty"`

	// Images holds image URLs found in this message's content parts. The
	// upstream agent body accepts them via image_urls /
	// chat_context.imageUrls, so dropping them makes an image-only request
	// fail with "no user message in request".
	Images []string `json:"-"`

	// Expanded carries tool_use/tool_result blocks embedded in a content array.
	// CPA normally translates those into dedicated messages, but Claude Code
	// and some Harness bridges can preserve the Anthropic block form.
	Expanded []openAIMessage `json:"-"`
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
		ReasoningSig     string          `json:"reasoning_content_signature"`
		ToolError        bool            `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	m.Role = probe.Role
	m.ToolCalls, m.ToolCallID, m.Name, m.ReasoningContent = probe.ToolCalls, probe.ToolCallID, probe.Name, probe.ReasoningContent
	m.ReasoningSig, m.ToolError = probe.ReasoningSig, probe.ToolError
	m.Content, m.Images, m.Expanded = parseMessageContent(probe.Role, probe.Content)
	return nil
}

// contentText flattens an OpenAI content value into plain text.
func contentParts(raw json.RawMessage) (string, []string) {
	text, images, _ := parseContentParts(raw)
	return text, images
}

// parseContentParts flattens text/image parts and expands Anthropic-style
// tool_use/tool_result blocks in their original order.
func parseContentParts(raw json.RawMessage) (string, []string, []openAIMessage) {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || string(v) == "null" {
		return "", nil, nil
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s, nil, nil
		}
		return "", nil, nil
	}
	if v[0] != '[' {
		return "", nil, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(v, &parts) != nil {
		return "", nil, nil
	}
	var out []string
	var images []string
	var expanded []openAIMessage
	for _, part := range parts {
		var p struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Content   json.RawMessage `json:"content"`
			Input     string          `json:"input_text"`
			Output    string          `json:"output_text"`
			ImageURL  json.RawMessage `json:"image_url"`
			FileData  json.RawMessage `json:"file_data"`
			Source    json.RawMessage `json:"source"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			InputJSON json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Signature string          `json:"signature"`
		}
		if json.Unmarshal(part, &p) != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "tool_use", "function_call":
			args := "{}"
			if len(bytes.TrimSpace(p.InputJSON)) > 0 {
				rawArgs := bytes.TrimSpace(p.InputJSON)
				var text string
				if json.Unmarshal(rawArgs, &text) == nil {
					args = text
				} else {
					args = string(rawArgs)
				}
			}
			calls, _ := json.Marshal([]any{map[string]any{
				"id": p.ID, "type": "function",
				"function": map[string]any{"name": p.Name, "arguments": args},
			}})
			expanded = append(expanded, openAIMessage{Role: "assistant", ToolCalls: calls})
			continue
		case "tool_result", "function_call_output":
			content, nestedImages, nested := parseContentParts(p.Content)
			if content == "" && len(p.Content) > 0 && bytes.TrimSpace(p.Content)[0] != '[' {
				var s string
				if json.Unmarshal(p.Content, &s) == nil {
					content = s
				} else {
					content = string(bytes.TrimSpace(p.Content))
				}
			}
			expanded = append(expanded, openAIMessage{
				Role: "tool", Content: content, Images: nestedImages,
				ToolCallID: firstNonEmpty(p.ToolUseID, p.ID), ToolError: p.IsError,
				Expanded: nested,
			})
			continue
		case "thinking", "reasoning", "reasoning_text":
			if p.Thinking != "" || p.Text != "" {
				expanded = append(expanded, openAIMessage{
					Role: "assistant", ReasoningContent: firstNonEmpty(p.Thinking, p.Text),
					ReasoningSig: p.Signature,
				})
			}
			continue
		}
		if url := imageURLFromPart(p.Type, p.ImageURL, p.FileData); url != "" {
			images = append(images, url)
			continue
		}
		if url := imageURLFromAnthropicPart(p.Source); url != "" {
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
			nested, nestedImages, nestedExpanded := parseContentParts(p.Content)
			if nested != "" {
				out = append(out, nested)
				images = append(images, nestedImages...)
			}
			expanded = append(expanded, nestedExpanded...)
		}
	}
	return strings.Join(out, "\n"), images, expanded
}

func parseMessageContent(role string, raw json.RawMessage) (string, []string, []openAIMessage) {
	text, images, expanded := parseContentParts(raw)
	if len(expanded) == 0 {
		return text, images, nil
	}

	// Split mixed text/tool arrays around tool blocks. This preserves the
	// current user input while emitting a valid call/result sequence.
	var out []openAIMessage
	buffer := openAIMessage{Role: role, Content: text, Images: images}
	for _, item := range expanded {
		switch item.Role {
		case "assistant":
			if strings.EqualFold(role, "assistant") && len(item.ToolCalls) > 0 {
				buffer.ToolCalls = item.ToolCalls
				buffer.ReasoningContent = firstNonEmpty(buffer.ReasoningContent, item.ReasoningContent)
				buffer.ReasoningSig = firstNonEmpty(buffer.ReasoningSig, item.ReasoningSig)
				out = append(out, buffer)
				buffer = openAIMessage{Role: role}
				continue
			}
			if buffer.Content != "" || len(buffer.Images) > 0 {
				out = append(out, buffer)
			}
			out = append(out, item)
			buffer = openAIMessage{Role: role}
		case "tool":
			if buffer.Content != "" || len(buffer.Images) > 0 {
				out = append(out, buffer)
			}
			out = append(out, item)
			buffer = openAIMessage{Role: role}
		}
	}
	if buffer.Content != "" || len(buffer.Images) > 0 {
		out = append(out, buffer)
	}
	return text, images, out
}

func imageURLFromAnthropicPart(raw json.RawMessage) string {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || v[0] != '{' {
		return ""
	}
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(v, &source) != nil {
		return ""
	}
	if source.URL != "" {
		return strings.TrimSpace(source.URL)
	}
	if source.Type == "base64" && source.MediaType != "" && source.Data != "" {
		return "data:" + source.MediaType + ";base64," + source.Data
	}
	return ""
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
	Functions           json.RawMessage `json:"functions"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	MaxOutputTokens     *int            `json:"max_output_tokens"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	PreserveThinking    *bool           `json:"preserve_thinking"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	TopK                *int            `json:"top_k"`
	Stop                json.RawMessage `json:"stop"`
	CustomContext       json.RawMessage `json:"custom_context"`
	ClientSessionID     string          `json:"-"`
	TaskID              string          `json:"-"`
	NoContextCompaction bool            `json:"-"`

	// Input carries the Responses API form when the caller (or CPA) forwards
	// it directly: "input" may be a string or an array of typed items.
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
	System       json.RawMessage `json:"system"`

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
	var out []openAIMessage
	for _, m := range r.Messages {
		if len(m.Expanded) > 0 {
			out = append(out, m.Expanded...)
			continue
		}
		out = append(out, m)
	}
	if len(out) > 0 {
		return out
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
	// Responses API represents calls, reasoning and outputs as top-level items.
	var msgs []openAIMessage
	asMessages := true
	for _, item := range items {
		var typed struct {
			Type             string          `json:"type"`
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			Summary          json.RawMessage `json:"summary"`
			EncryptedContent string          `json:"encrypted_content"`
			CallID           string          `json:"call_id"`
			Name             string          `json:"name"`
			Arguments        string          `json:"arguments"`
			Input            json.RawMessage `json:"input"`
			Output           json.RawMessage `json:"output"`
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
		case "custom_tool_call":
			args := "{}"
			if raw := bytes.TrimSpace(typed.Input); len(raw) > 0 {
				if raw[0] == '"' {
					_ = json.Unmarshal(raw, &args)
				} else {
					args = string(raw)
				}
			}
			calls, _ := json.Marshal([]any{map[string]any{"id": typed.CallID, "type": "function", "function": map[string]any{"name": typed.Name, "arguments": args}}})
			msgs = append(msgs, openAIMessage{Role: "assistant", ToolCalls: calls})
			continue
		case "custom_tool_call_output":
			content, images := contentParts(typed.Output)
			msgs = append(msgs, openAIMessage{Role: "tool", ToolCallID: typed.CallID, Content: content, Images: images})
			continue
		case "reasoning":
			text, images, expanded := parseContentParts(typed.Content)
			summary, summaryImages, summaryExpanded := parseContentParts(typed.Summary)
			msg := openAIMessage{
				Role: "assistant", Content: text,
				ReasoningContent: firstNonEmpty(summary, text),
				ReasoningSig:     typed.EncryptedContent,
				Images:           append(images, summaryImages...),
			}
			if msg.Content != "" || msg.ReasoningContent != "" || len(msg.Images) > 0 {
				msgs = append(msgs, msg)
			}
			msgs = append(msgs, append(expanded, summaryExpanded...)...)
			continue
		case "message":
			var m openAIMessage
			if json.Unmarshal(item, &m) == nil && m.Role != "" {
				msgs = append(msgs, m)
				continue
			}
		case "input_text", "output_text", "text":
			if typed.Content != nil {
				text, images, expanded := parseContentParts(typed.Content)
				msgs = append(msgs, openAIMessage{Role: firstNonEmpty(typed.Role, "user"), Content: text, Images: images, Expanded: expanded})
			} else if typed.Output != nil {
				msg := openAIMessage{Role: firstNonEmpty(typed.Role, "user")}
				msg.Content, msg.Images, msg.Expanded = parseMessageContent(msg.Role, typed.Output)
				msgs = append(msgs, msg)
			}
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

func requestMessages(req *openAIRequest) []openAIMessage {
	if req == nil {
		return nil
	}
	messages := append([]openAIMessage(nil), req.effectiveMessages()...)
	if len(bytes.TrimSpace(req.System)) > 0 {
		text, images, expanded := parseContentParts(req.System)
		if text != "" || len(images) > 0 {
			messages = append([]openAIMessage{{Role: "system", Content: text, Images: images, Expanded: expanded}}, messages...)
		}
	}
	if req.Instructions != "" {
		messages = append([]openAIMessage{{Role: "system", Content: req.Instructions}}, messages...)
	}
	return messages
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

// buildQoderBody renders the current Qoder CLI agent_chat_generation body.
// The field set and defaults mirror CLI 1.1.89's remote-chat builder rather
// than the older captured template.
func buildQoderBody(req *openAIRequest, modelKey, userType string, sa *storedAuth) ([]byte, error) {
	messages := requestMessages(req)

	meta, hasMeta := metadataForModel(sa, modelKey)
	reasoningEffort, reasoningEnabled, err := resolveReasoning(sa, modelKey, req.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	if sa != nil {
		for i := range messages {
			if len(messages[i].Images) == 0 {
				continue
			}
			if !hasMeta || !meta.vision() {
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

	modelReasoning := false
	if hasMeta {
		modelReasoning = rawBool(meta.IsReasoning, false)
		if enabled, ok := meta.ThinkingConfig["enabled"]; ok && enabled.IsDefault {
			modelReasoning = true
		}
		if disabled, ok := meta.ThinkingConfig["disabled"]; ok && disabled.IsDefault {
			modelReasoning = false
		}
	}
	if reasoningEnabled != nil {
		modelReasoning = *reasoningEnabled
	}

	contextWindow := effectiveContextWindow(sa, modelKey)
	maxTokens := requestedMaxOutputTokens(req, meta)
	displayName := modelKey
	format := "openai"
	source := "system"
	if hasMeta {
		displayName = firstNonEmpty(strings.TrimSpace(meta.DisplayName), displayName)
		format = firstNonEmpty(strings.TrimSpace(meta.Format), format)
		source = firstNonEmpty(strings.TrimSpace(meta.Source), source)
	}

	parameters := map[string]any{"max_tokens": maxTokens}
	if req.Temperature != nil {
		parameters["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		parameters["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		parameters["top_k"] = *req.TopK
	}
	if len(bytes.TrimSpace(req.Stop)) > 0 {
		var stop any
		if json.Unmarshal(req.Stop, &stop) == nil {
			parameters["stop"] = stop
		}
	}
	if reasoningEffort != "" {
		parameters["reasoning_effort"] = reasoningEffort
	} else if reasoningEnabled != nil && !*reasoningEnabled {
		parameters["reasoning_effort"] = "none"
	}
	if reasoningEffort != "" || reasoningEnabled != nil {
		parameters["enable_thinking"] = modelReasoning
	}
	if req.PreserveThinking != nil {
		parameters["preserve_thinking"] = *req.PreserveThinking
	}
	if contextWindow > 0 {
		parameters["context_length"] = contextWindow
	}
	if choice := normalizeQoderToolChoice(req.ToolChoice); choice != nil {
		parameters["tool_choice"] = choice
	}

	modelConfig := map[string]any{
		"key":              modelKey,
		"display_name":     displayName,
		"model":            "",
		"is_vl":            hasMeta && meta.vision(),
		"is_reasoning":     modelReasoning,
		"max_input_tokens": contextWindow,
		"source":           source,
		"format":           format,
		"api_key":          "",
		"url":              "",
	}

	var out []any
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "developer" {
			role = "system"
		}
		switch role {
		case "system", "user", "assistant", "tool", "function":
		default:
			role = "system"
		}
		content := messageContent(m)
		item := map[string]any{"role": role, "content": content}
		if _, ok := content.([]any); ok {
			// CLI 1.1.89 emits both fields when a message has image parts.
			item["contents"] = content
		}
		if len(m.ToolCalls) > 0 {
			if calls := normalizeQoderToolCalls(m.ToolCalls); len(calls) > 0 {
				item["tool_calls"] = calls
			}
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			item["name"] = sanitizeQoderText(m.Name)
		}
		if m.ReasoningContent != "" {
			item["reasoning_content"] = sanitizeQoderText(m.ReasoningContent)
		}
		if m.ReasoningSig != "" {
			item["reasoning_content_signature"] = m.ReasoningSig
		}
		if m.ToolError {
			item["is_error"] = true
		}
		out = append(out, item)
	}
	sanitizeQoderMessages(out)

	nid := uuid.NewString()
	requestSetID := uuid.NewString()
	taskID := firstNonEmpty(strings.TrimSpace(req.TaskID), "common")
	businessName := prompt
	if runes := []rune(businessName); len(runes) > 30 {
		businessName = string(runes[:30])
	}
	sessionType := "qoderclicn"
	if sa != nil && sa.region() == regionIntl {
		sessionType = "qodercli"
	}

	body := map[string]any{
		"request_id":       nid,
		"chat_record_id":   nid,
		"request_set_id":   requestSetID,
		"session_id":       referenceSessionID(sa, modelKey, req.ClientSessionID),
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"agent_id":         "agent_common",
		"aliyun_user_type": "",
		"session_type":     sessionType,
		"task_id":          taskID,
		"system":           nil,
		"chat_context": map[string]any{
			"text":     prompt,
			"features": []any{},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": modelReasoning},
				"originalContent": prompt,
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"messages":     out,
		"tools":        normalizeQoderTools(req.Tools, req.Functions),
		"parameters":   parameters,
		"model_config": modelConfig,
		"business": map[string]any{
			"product":  "cli",
			"version":  qoderCLIWireVersion,
			"type":     "agent",
			"id":       requestSetID,
			"name":     businessName,
			"begin_at": time.Now().UnixMilli(),
			"stage":    "start",
		},
	}
	if len(bytes.TrimSpace(req.CustomContext)) > 0 {
		var custom any
		if json.Unmarshal(req.CustomContext, &custom) == nil {
			body["custom_context"] = custom
		}
	}
	// Keep the argument in the signature for all existing callers; the CLI
	// derives user type from signed identity rather than sending it.
	_ = userType
	return json.Marshal(body)
}

func effectiveContextWindow(sa *storedAuth, modelKey string) int64 {
	meta, ok := metadataForModel(sa, modelKey)
	if !ok {
		return 0
	}
	if sa != nil && useMaxContext(sa.region()) {
		return meta.maxContextLength()
	}
	return meta.contextLength()
}

func requestedMaxOutputTokens(req *openAIRequest, meta modelCatalogEntry) int {
	const defaultMaxOutput = 32000
	value := 0
	for _, candidate := range []*int{req.MaxTokens, req.MaxCompletionTokens, req.MaxOutputTokens} {
		if candidate != nil && *candidate > 0 {
			value = *candidate
		}
	}
	if declared := int(meta.maxCompletion()); declared > 0 && (value == 0 || value > declared) {
		value = declared
	}
	if value <= 0 {
		value = defaultMaxOutput
	}
	return value
}

func normalizeQoderTools(primary, legacy json.RawMessage) []any {
	raw := primary
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = legacy
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return []any{}
	}
	var source []map[string]any
	if json.Unmarshal(raw, &source) != nil {
		return []any{}
	}
	out := make([]any, 0, len(source))
	for _, tool := range source {
		name, description := "", ""
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}}
		if fn, ok := tool["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			description, _ = fn["description"].(string)
			if v, ok := fn["parameters"]; ok {
				parameters = v
			}
		} else {
			name, _ = tool["name"].(string)
			description, _ = tool["description"].(string)
			if v, ok := tool["parameters"]; ok {
				parameters = v
			} else if v, ok := tool["input_schema"]; ok {
				parameters = v
			}
		}
		name, description = strings.TrimSpace(name), strings.TrimSpace(description)
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": sanitizeQoderText(description),
				"parameters":  parameters,
			},
		})
	}
	return out
}

func normalizeQoderToolChoice(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return v
	case map[string]any:
		typ, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "auto", "required", "none":
			return strings.ToLower(strings.TrimSpace(typ))
		case "function":
			if fn, ok := v["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); strings.TrimSpace(name) != "" {
					return strings.TrimSpace(name)
				}
			}
			if name, _ := v["name"].(string); strings.TrimSpace(name) != "" {
				return strings.TrimSpace(name)
			}
		}
	}
	return nil
}

func normalizeQoderToolCalls(raw json.RawMessage) []any {
	var calls []map[string]any
	if json.Unmarshal(raw, &calls) != nil {
		return nil
	}
	out := make([]any, 0, len(calls))
	for _, call := range calls {
		id, _ := call["id"].(string)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		arguments, _ := fn["arguments"].(string)
		if arguments == "" {
			if nested, ok := fn["arguments"].(map[string]any); ok {
				b, _ := json.Marshal(nested)
				arguments = string(b)
			} else {
				arguments = "{}"
			}
		}
		out = append(out, map[string]any{
			"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": arguments},
		})
	}
	return out
}

func sanitizeQoderText(s string) string {
	return contextrules.Sanitize(s)
}

// sanitizeQoderMessages 净化消息数组中所有文本字段。
func sanitizeQoderMessages(msgs []any) {
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if r := sanitizeQoderText(c); r != c {
				msg["content"] = r
			}
		case []any:
			for _, part := range c {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok {
					if r := sanitizeQoderText(t); r != t {
						p["text"] = r
					}
				}
			}
		}
	}
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
