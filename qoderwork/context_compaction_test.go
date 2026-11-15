package main

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func clearCompactionLocks() {
	compactionLocks.Lock()
	compactionLocks.entries = make(map[string]*compactionKeyedLock)
	compactionLocks.Unlock()
}

func TestMaybeCompactUsesCLIThresholdAndCachesCoveredPrefix(t *testing.T) {
	clearCompactionLocks()
	compactionCache.Lock()
	compactionCache.entries = make(map[string]*compactionCacheEntry)
	compactionCache.Unlock()
	qoderCompactionStats = compactionStats{}
	setCompactionSettings(true, 0, 4096, "")
	defer setCompactionSettings(true, 0, defaultCompactionMaxTokens, "")

	sa, model := testCatalogAuth(t, "cmpmodel", "Compaction Test", 200_000, 32000)
	oldMessages := []openAIMessage{
		{Role: "system", Content: "You are Codex, a coding agent running in the Codex CLI."},
	}
	large := strings.Repeat("abcdefghij", 8500) // approximately 21k tokens per message
	for i := 0; i < 5; i++ {
		oldMessages = append(oldMessages,
			openAIMessage{Role: "user", Content: large},
			openAIMessage{Role: "assistant", Content: "completed " + large},
		)
	}
	oldMessages = append(oldMessages, openAIMessage{Role: "user", Content: "current request"})
	rawMessages := append([]openAIMessage(nil), oldMessages...)
	req := &openAIRequest{
		Model:           model,
		Messages:        oldMessages,
		ClientSessionID: "compaction-session",
	}
	calls := 0
	oldRequest := requestQoderCompactionFn
	requestQoderCompactionFn = func(context.Context, *openAIRequest, []openAIMessage, []openAIMessage, compactionSettings, *storedAuth, string) (string, usage.Detail, error) {
		calls++
		return "## Primary Request and Intent\n- finish the implementation", usage.Detail{TotalTokens: 77}, nil
	}
	defer func() { requestQoderCompactionFn = oldRequest }()

	result := maybeCompactRequest(context.Background(), req, model, model, sa)
	if !result.Compacted || result.Source != "summary" || calls != 1 {
		t.Fatalf("first compaction = %#v calls=%d", result, calls)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("compacted messages = %#v", req.Messages)
	}
	if req.Messages[0].Role != "system" || !strings.Contains(req.Messages[1].Content, "<compacted-summary>") || req.Messages[2].Content != "current request" {
		t.Fatalf("checkpoint structure = %#v", req.Messages)
	}

	nextMessages := append([]openAIMessage(nil), rawMessages...)
	nextMessages = append(nextMessages,
		openAIMessage{Role: "assistant", Content: "current answer"},
		openAIMessage{Role: "user", Content: "next request"},
	)
	next := &openAIRequest{
		Model:           model,
		Messages:        nextMessages,
		ClientSessionID: "compaction-session",
	}
	cached := maybeCompactRequest(context.Background(), next, model, model, sa)
	if !cached.Compacted || cached.Source != "cache" || calls != 1 {
		t.Fatalf("cached compaction = %#v calls=%d", cached, calls)
	}
	if next.Messages[len(next.Messages)-1].Content != "next request" {
		t.Fatalf("latest user was not retained: %#v", next.Messages)
	}
}

func TestCompactionFailureLeavesOriginalRequest(t *testing.T) {
	clearCompactionLocks()
	setCompactionSettings(true, 0, 4096, "")
	sa, model := testCatalogAuth(t, "failmodel", "Failure Test", 200_000, 32000)
	large := strings.Repeat("x", 700_000)
	original := []openAIMessage{
		{Role: "system", Content: "system"},
		{Role: "user", Content: large},
		{Role: "assistant", Content: large},
		{Role: "user", Content: "latest"},
	}
	req := &openAIRequest{Model: model, Messages: append([]openAIMessage(nil), original...), ClientSessionID: "failure-session"}
	oldRequest := requestQoderCompactionFn
	requestQoderCompactionFn = func(context.Context, *openAIRequest, []openAIMessage, []openAIMessage, compactionSettings, *storedAuth, string) (string, usage.Detail, error) {
		return "", usage.Detail{}, context.DeadlineExceeded
	}
	defer func() { requestQoderCompactionFn = oldRequest }()
	result := maybeCompactRequest(context.Background(), req, model, model, sa)
	if result.Compacted || result.Error == "" {
		t.Fatalf("result = %#v", result)
	}
	if len(req.Messages) != len(original) || req.Messages[3].Content != "latest" {
		t.Fatalf("original request changed: %#v", req.Messages)
	}
}

func TestParseQoderCompactionSummary(t *testing.T) {
	raw := "<analysis>private</analysis>\n<summary>\n## Current Work\n- implement\n</summary>"
	if got := parseQoderCompactionSummary(raw); !strings.Contains(got, "## Current Work") || strings.Contains(got, "private") {
		t.Fatalf("summary = %q", got)
	}
	if got := parseQoderCompactionSummary("<analysis>private</analysis>\nplain summary"); got != "plain summary" {
		t.Fatalf("plain fallback = %q", got)
	}
}

func TestQoderAutoCompactPercentOverride(t *testing.T) {
	t.Setenv("QODER_AUTOCOMPACT_WINDOW", "500000")
	t.Setenv("QODER_AUTOCOMPACT_PCT_OVERRIDE", "60")
	if got, want := qoderAutoCompactThreshold(1_000_000, 32000, 0), 288000; got != want {
		t.Fatalf("threshold = %d want %d", got, want)
	}
}
