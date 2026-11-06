package main

import (
	"encoding/json"
	"strings"
)

var thinkingTags = []string{"<thinking>", "</thinking>", "<think>", "</think>"}

type thinkingSegment struct {
	reasoning bool
	text      string
}
type thinkingParser struct {
	buffer, native, closing string
	reasoning               bool
}

func trailingTagPrefix(s string, tags []string) int {
	best := 0
	for _, tag := range tags {
		limit := len(tag) - 1
		if len(s) < limit {
			limit = len(s)
		}
		for n := limit; n > best; n-- {
			if strings.HasSuffix(s, tag[:n]) {
				best = n
				break
			}
		}
	}
	return best
}
func trimThinkingSeparator(s string) string {
	if strings.HasPrefix(s, "\n\n") {
		return s[2:]
	}
	return strings.TrimPrefix(s, "\n")
}
func earliestThinkingTag(s string, tags []string) (int, string) {
	at := -1
	found := ""
	for _, tag := range tags {
		if i := strings.Index(s, tag); i >= 0 && (at < 0 || i < at) {
			at, found = i, tag
		}
	}
	return at, found
}
func (p *thinkingParser) pushNative(s string) string {
	p.native += s
	var out strings.Builder
	for p.native != "" {
		if i, tag := earliestThinkingTag(p.native, thinkingTags); i >= 0 {
			out.WriteString(p.native[:i])
			p.native = p.native[i+len(tag):]
			continue
		}
		held := trailingTagPrefix(p.native, thinkingTags)
		out.WriteString(p.native[:len(p.native)-held])
		p.native = p.native[len(p.native)-held:]
		break
	}
	return out.String()
}
func (p *thinkingParser) finishNative() string {
	value := p.native
	p.native = ""
	for _, tag := range thinkingTags {
		if strings.HasPrefix(tag, value) {
			return ""
		}
	}
	return value
}
func (p *thinkingParser) pushContent(s string) []thinkingSegment {
	var out []thinkingSegment
	if pending := p.finishNative(); pending != "" {
		out = append(out, thinkingSegment{true, pending})
	}
	p.buffer += s
	for p.buffer != "" {
		if p.reasoning {
			if i := strings.Index(p.buffer, p.closing); i >= 0 {
				if i > 0 {
					out = append(out, thinkingSegment{true, p.buffer[:i]})
				}
				p.buffer = trimThinkingSeparator(p.buffer[i+len(p.closing):])
				p.reasoning = false
				continue
			}
			held := trailingTagPrefix(p.buffer, []string{p.closing})
			safe := len(p.buffer) - held
			if safe > 0 {
				out = append(out, thinkingSegment{true, p.buffer[:safe]})
			}
			p.buffer = p.buffer[safe:]
			break
		}
		openAt, open := earliestThinkingTag(p.buffer, []string{"<thinking>", "<think>"})
		closeAt, closeTag := earliestThinkingTag(p.buffer, []string{"</thinking>", "</think>"})
		if openAt >= 0 && (closeAt < 0 || openAt < closeAt) {
			if openAt > 0 {
				out = append(out, thinkingSegment{false, p.buffer[:openAt]})
			}
			p.buffer = p.buffer[openAt+len(open):]
			p.reasoning = true
			p.closing = "</think>"
			if open == "<thinking>" {
				p.closing = "</thinking>"
			}
			continue
		}
		if closeAt >= 0 {
			if closeAt > 0 {
				out = append(out, thinkingSegment{false, p.buffer[:closeAt]})
			}
			p.buffer = trimThinkingSeparator(p.buffer[closeAt+len(closeTag):])
			continue
		}
		held := trailingTagPrefix(p.buffer, thinkingTags)
		safe := len(p.buffer) - held
		if safe > 0 {
			out = append(out, thinkingSegment{false, p.buffer[:safe]})
		}
		p.buffer = p.buffer[safe:]
		break
	}
	return out
}
func (p *thinkingParser) finish() []thinkingSegment {
	var out []thinkingSegment
	if s := p.finishNative(); s != "" {
		out = append(out, thinkingSegment{true, s})
	}
	if p.buffer != "" {
		out = append(out, thinkingSegment{p.reasoning, p.buffer})
	}
	p.buffer = ""
	return out
}

// qoderChunkNormalizer preserves role/tools/usage while separating embedded
// thinking tags into OpenAI reasoning_content, including split tag fragments.
type qoderChunkNormalizer struct{ parsers map[int]*thinkingParser }

func (n *qoderChunkNormalizer) parser(index int) *thinkingParser {
	if n.parsers == nil {
		n.parsers = map[int]*thinkingParser{}
	}
	if n.parsers[index] == nil {
		n.parsers[index] = &thinkingParser{}
	}
	return n.parsers[index]
}
func addSegments(delta map[string]any, segments []thinkingSegment) {
	var text, reason strings.Builder
	for _, segment := range segments {
		if segment.reasoning {
			reason.WriteString(segment.text)
		} else {
			text.WriteString(segment.text)
		}
	}
	if text.Len() > 0 {
		delta["content"] = text.String()
	}
	if reason.Len() > 0 {
		delta["reasoning_content"] = reason.String()
	}
}
func (n *qoderChunkNormalizer) normalize(raw string) string {
	var chunk map[string]any
	if json.Unmarshal([]byte(raw), &chunk) != nil {
		return raw
	}
	choices, _ := chunk["choices"].([]any)
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		index := 0
		if v, ok := choice["index"].(float64); ok {
			index = int(v)
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		p := n.parser(index)
		var segments []thinkingSegment
		if v, ok := delta["reasoning_content"].(string); ok {
			delete(delta, "reasoning_content")
			if s := p.pushNative(v); s != "" {
				segments = append(segments, thinkingSegment{true, s})
			}
		}
		if v, ok := delta["content"].(string); ok {
			delete(delta, "content")
			segments = append(segments, p.pushContent(v)...)
		}
		if finish, _ := choice["finish_reason"].(string); finish != "" {
			segments = append(segments, p.finish()...)
		}
		addSegments(delta, segments)
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return raw
	}
	return string(data)
}
func (n *qoderChunkNormalizer) flush(model string) []string {
	var out []string
	for index, p := range n.parsers {
		segments := p.finish()
		if len(segments) == 0 {
			continue
		}
		delta := map[string]any{}
		addSegments(delta, segments)
		chunk := map[string]any{"object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": index, "delta": delta}}}
		if b, err := json.Marshal(chunk); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}
