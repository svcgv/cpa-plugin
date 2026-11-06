package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestThinkingParserSeparatesSplitTags(t *testing.T) {
	p := &thinkingParser{}
	var got []thinkingSegment
	for _, s := range []string{"A <thi", "nk>reason", "ing</thi", "nk>\n\nanswer"} {
		got = append(got, p.pushContent(s)...)
	}
	got = append(got, p.finish()...)
	var text, reason strings.Builder
	for _, s := range got {
		if s.reasoning {
			reason.WriteString(s.text)
		} else {
			text.WriteString(s.text)
		}
	}
	if text.String() != "A answer" || reason.String() != "reasoning" {
		t.Fatalf("text=%q reasoning=%q", text.String(), reason.String())
	}
}

func TestThinkingNormalizerPreservesToolsAndFinish(t *testing.T) {
	n := &qoderChunkNormalizer{}
	chunks := []string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":"<thi"}}]}`, `{"choices":[{"index":0,"delta":{"content":"nk>work</think>\nanswer","tool_calls":[{"index":0,"id":"call"}]},"finish_reason":"tool_calls"}]}`}
	var text, reason string
	var tool bool
	var finish string
	for _, raw := range chunks {
		out := n.normalize(raw)
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []any  `json:"tool_calls"`
				} `json:"delta"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(out), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, c := range chunk.Choices {
			text += c.Delta.Content
			reason += c.Delta.Reasoning
			tool = tool || len(c.Delta.ToolCalls) > 0
			if c.Finish != "" {
				finish = c.Finish
			}
		}
	}
	if text != "answer" || reason != "work" || !tool || finish != "tool_calls" {
		t.Fatalf("text=%q reasoning=%q tool=%v finish=%s", text, reason, tool, finish)
	}
}
