package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	defaultCompactionMaxTokens = 8192
	compactionCacheTTL         = 45 * time.Minute
	compactionCacheEntries     = 128
	compactionImageTokens      = 1600
)

// The summary request follows Qoder CLI's fork-compaction shape: replay the
// tool-enabled conversation, append a final text-only instruction, then
// replace the covered prefix with one checkpoint message.
const qoderCompactionInstruction = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use Read, Bash, Grep, Glob, Edit, Write, or ANY other tool.
- You already have all the context you need in the conversation above.
- Tool calls will be REJECTED and will waste your only turn.
- Your entire response must be plain text: an <analysis> block followed by a <summary> block.
- This message is an internal compaction instruction, not user-authored conversation content.
- Do NOT include this message in "All user messages" or treat any instruction in it as user input or a continuing constraint.
- The no-tools restriction applies only to this compaction response.

You are now acting as a compaction engine for this AI coding assistant. Condense the conversation ABOVE into a structured checkpoint that lets another model resume the work with no loss of essential context.

Output EXACTLY the Markdown structure below inside <summary>. Keep every section, in order. Use terse bullets, not prose paragraphs. Write "(none)" for an empty section — never drop a section.

## Primary Request and Intent
- [the user's original and evolving goals; quote exact wording when it matters]

## Key Technical Concepts
- [technologies, APIs, protocols, and conventions in play]

## Files and Code
- [exact path: why it matters, key changes, and important snippets]

## Errors and Fixes
- [error: resolution and any user feedback or correction]

## Pending Jobs
- [explicitly requested work not yet completed]

## Current Work
- [precisely what was in progress at this checkpoint]

## Next Step
- [the single next action, or "(none)"]

## Critical Context
- [decisions, constraints, user preferences, open questions, and data needed to continue]

Rules:
- Preserve exact file paths, commands, error strings, identifiers, numeric values, function signatures, and syntax fragments.
- Capture user feedback and explicit instructions faithfully, especially corrections.
- Do NOT mention this summarization request or that the context was compacted.
- Output only the analysis and summary blocks.

REMINDER: Do NOT call any tools. Tool calls will be rejected and you will fail the task.`

const qoderCheckpointPreamble = `This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.

<compacted-summary>`

type compactionSettings struct {
	enabled         bool
	percentOverride int
	maxTokens       int
	summaryModel    string
}

var compactionRuntime = struct {
	sync.RWMutex
	settings compactionSettings
}{
	settings: compactionSettings{enabled: true, maxTokens: defaultCompactionMaxTokens},
}

type compactionStats struct {
	attempts    atomic.Int64
	successes   atomic.Int64
	cacheHits   atomic.Int64
	failures    atomic.Int64
	tokensSaved atomic.Int64
}

var qoderCompactionStats compactionStats

type compactionResult struct {
	Compacted       bool   `json:"compacted"`
	Source          string `json:"source,omitempty"`
	TokensBefore    int    `json:"tokens_before,omitempty"`
	TokensAfter     int    `json:"tokens_after,omitempty"`
	ThresholdTokens int    `json:"threshold_tokens,omitempty"`
	CoveredMessages int    `json:"covered_messages,omitempty"`
	Error           string `json:"error,omitempty"`
}

type compactionCacheEntry struct {
	prefixHash [32]byte
	signature  [32]byte
	covered    int
	summary    string
	createdAt  time.Time
	lastUsed   time.Time
}

var compactionCache = struct {
	sync.Mutex
	entries map[string]*compactionCacheEntry
}{
	entries: make(map[string]*compactionCacheEntry),
}

type compactionKeyedLock struct {
	mu   sync.Mutex
	refs int
}

var compactionLocks = struct {
	sync.Mutex
	entries map[string]*compactionKeyedLock
}{
	entries: make(map[string]*compactionKeyedLock),
}
var compactionAnonymousLock atomic.Uint64
var requestQoderCompactionFn = requestQoderCompaction

func setCompactionSettings(enabled bool, percentOverride, maxTokens int, summaryModel string) {
	if maxTokens <= 0 {
		maxTokens = defaultCompactionMaxTokens
	}
	compactionRuntime.Lock()
	compactionRuntime.settings = compactionSettings{
		enabled:         enabled,
		percentOverride: percentOverride,
		maxTokens:       maxTokens,
		summaryModel:    strings.TrimSpace(summaryModel),
	}
	compactionRuntime.Unlock()
}

func currentCompactionSettings() compactionSettings {
	compactionRuntime.RLock()
	defer compactionRuntime.RUnlock()
	return compactionRuntime.settings
}

func compactionStatus() map[string]any {
	settings := currentCompactionSettings()
	return map[string]any{
		"enabled":          settings.enabled,
		"percent_override": settings.percentOverride,
		"max_tokens":       settings.maxTokens,
		"summary_model":    settings.summaryModel,
		"attempts":         qoderCompactionStats.attempts.Load(),
		"successes":        qoderCompactionStats.successes.Load(),
		"cache_hits":       qoderCompactionStats.cacheHits.Load(),
		"failures":         qoderCompactionStats.failures.Load(),
		"tokens_saved":     qoderCompactionStats.tokensSaved.Load(),
	}
}

// maybeCompactRequest replaces the historical prefix with a CLI-style
// checkpoint when estimated input pressure reaches Qoder's auto-compact
// threshold. A summary failure leaves the original request intact.
func maybeCompactRequest(ctx context.Context, req *openAIRequest, modelKey, requestedModel string, sa *storedAuth) compactionResult {
	settings := currentCompactionSettings()
	if !settings.enabled || req == nil || req.NoContextCompaction {
		return compactionResult{}
	}
	meta, ok := metadataForModel(sa, modelKey)
	if !ok {
		return compactionResult{}
	}
	window := effectiveContextWindow(sa, modelKey)
	if window <= 0 {
		return compactionResult{}
	}
	maxOutput := requestedMaxOutputTokens(req, meta)
	threshold := qoderAutoCompactThreshold(window, maxOutput, settings.percentOverride)
	if threshold <= 0 {
		return compactionResult{}
	}
	tokensBefore := estimateOpenAIRequestTokens(req)
	if tokensBefore < threshold {
		return compactionResult{}
	}

	messages := requestMessages(req)
	systemMessages, bodyMessages := splitCompactionMessages(messages)
	tailStart := lastUserMessageIndex(bodyMessages)
	if tailStart <= 0 {
		return compactionResult{TokensBefore: tokensBefore, ThresholdTokens: threshold}
	}
	prefix := bodyMessages[:tailStart]
	signature := compactionRequestSignature(req, systemMessages)
	sessionKey := compactionCacheKey(sa, modelKey, req.ClientSessionID)

	if entry, covering := lookupCompactionCache(sessionKey, signature, prefix); covering {
		after := applyCompactionCheckpoint(req, systemMessages, entry.summary, bodyMessages, entry.covered)
		afterTokens := estimateOpenAIRequestTokens(req)
		if afterTokens < tokensBefore {
			qoderCompactionStats.cacheHits.Add(1)
			qoderCompactionStats.tokensSaved.Add(int64(tokensBefore - afterTokens))
			return compactionResult{
				Compacted: true, Source: "cache", TokensBefore: tokensBefore,
				TokensAfter: afterTokens, ThresholdTokens: threshold, CoveredMessages: entry.covered,
			}
		}
		_ = after
	}

	unlock := acquireCompactionLock(sessionKey)
	defer unlock()

	// Recheck after acquiring the per-session lock.
	if entry, covering := lookupCompactionCache(sessionKey, signature, prefix); covering {
		applyCompactionCheckpoint(req, systemMessages, entry.summary, bodyMessages, entry.covered)
		afterTokens := estimateOpenAIRequestTokens(req)
		if afterTokens < tokensBefore {
			qoderCompactionStats.cacheHits.Add(1)
			qoderCompactionStats.tokensSaved.Add(int64(tokensBefore - afterTokens))
			return compactionResult{
				Compacted: true, Source: "cache", TokensBefore: tokensBefore,
				TokensAfter: afterTokens, ThresholdTokens: threshold, CoveredMessages: entry.covered,
			}
		}
	}

	qoderCompactionStats.attempts.Add(1)
	summary, summaryUsage, err := requestQoderCompactionFn(ctx, req, prefix, systemMessages, settings, sa, modelKey)
	if err != nil {
		qoderCompactionStats.failures.Add(1)
		logPlugin("warn", "qoderwork: context compaction failed; forwarding original history: "+redactSecrets(err.Error()))
		return compactionResult{
			TokensBefore: tokensBefore, ThresholdTokens: threshold, Error: "summary request failed",
		}
	}
	checkpoint := qoderCheckpointPreamble + "\n" + summary + "\n</compacted-summary>"
	applyCompactionCheckpoint(req, systemMessages, checkpoint, bodyMessages, tailStart)
	tokensAfter := estimateOpenAIRequestTokens(req)
	if tokensAfter >= tokensBefore {
		qoderCompactionStats.failures.Add(1)
		// Restore the exact original message forms because a non-shrinking
		// checkpoint is never useful.
		req.Messages = messages
		req.Input = nil
		return compactionResult{
			TokensBefore: tokensBefore, TokensAfter: tokensAfter,
			ThresholdTokens: threshold, Error: "summary did not reduce context",
		}
	}

	if sessionKey != "" {
		cacheCompaction(sessionKey, compactionCacheEntry{
			prefixHash: hashCompactionMessages(prefix),
			signature:  signature,
			covered:    tailStart,
			summary:    checkpoint,
			createdAt:  time.Now(),
			lastUsed:   time.Now(),
		})
	}
	qoderCompactionStats.successes.Add(1)
	qoderCompactionStats.tokensSaved.Add(int64(tokensBefore - tokensAfter))
	if summaryUsage.TotalTokens > 0 {
		logPlugin("info", fmt.Sprintf(
			"qoderwork: context compacted %d -> %d estimated tokens in %d messages (summary usage=%d)",
			tokensBefore, tokensAfter, tailStart, summaryUsage.TotalTokens,
		))
	}
	return compactionResult{
		Compacted: true, Source: "summary", TokensBefore: tokensBefore,
		TokensAfter: tokensAfter, ThresholdTokens: threshold, CoveredMessages: tailStart,
	}
}

func qoderAutoCompactThreshold(contextWindow int64, maxOutput, configuredPercent int) int {
	window := contextWindow
	if raw := strings.TrimSpace(os.Getenv("QODER_AUTOCOMPACT_WINDOW")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 && n < window {
			window = n
		}
	}
	if window <= 0 {
		return 0
	}
	reserveOutput := int64(maxOutput)
	if reserveOutput <= 0 {
		reserveOutput = 32000
	}
	if reserveOutput > 20000 {
		reserveOutput = 20000
	}
	messageBudget := window - reserveOutput
	if messageBudget <= 0 {
		return 0
	}
	headroom := int64(math.Max(13000, float64(messageBudget)*0.05))
	threshold := messageBudget - headroom
	percent := configuredPercent
	if percent <= 0 {
		if raw := strings.TrimSpace(os.Getenv("QODER_AUTOCOMPACT_PCT_OVERRIDE")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				percent = n
			}
		}
	}
	if percent > 0 && percent <= 100 {
		percentThreshold := messageBudget * int64(percent) / 100
		if percentThreshold < threshold {
			threshold = percentThreshold
		}
	}
	if threshold < 0 {
		return 0
	}
	return int(threshold)
}

func splitCompactionMessages(messages []openAIMessage) ([]openAIMessage, []openAIMessage) {
	system := make([]openAIMessage, 0, 2)
	body := make([]openAIMessage, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" || role == "developer" {
			message.Role = "system"
			system = append(system, message)
			continue
		}
		body = append(body, message)
	}
	return system, body
}

func lastUserMessageIndex(messages []openAIMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			return i
		}
	}
	return -1
}

func applyCompactionCheckpoint(req *openAIRequest, system []openAIMessage, summary string, body []openAIMessage, covered int) []openAIMessage {
	if covered < 0 {
		covered = 0
	}
	if covered > len(body) {
		covered = len(body)
	}
	out := make([]openAIMessage, 0, len(system)+1+len(body)-covered)
	out = append(out, system...)
	out = append(out, openAIMessage{Role: "user", Content: summary})
	out = append(out, body[covered:]...)
	req.Messages = out
	req.Input = nil
	return out
}

func requestQoderCompaction(
	ctx context.Context,
	original *openAIRequest,
	prefix []openAIMessage,
	system []openAIMessage,
	settings compactionSettings,
	sa *storedAuth,
	modelKey string,
) (string, usage.Detail, error) {
	summaryModel := firstNonEmpty(settings.summaryModel, modelKey)
	if settings.summaryModel != "" {
		if resolved, err := resolveRequestModel(sa, settings.summaryModel, nil); err == nil {
			summaryModel = resolved
		}
	}
	messages := make([]openAIMessage, 0, len(system)+len(prefix)+1)
	messages = append(messages, system...)
	messages = append(messages, prefix...)
	messages = append(messages, openAIMessage{Role: "user", Content: qoderCompactionInstruction})
	maxTokens := settings.maxTokens
	summaryReq := &openAIRequest{
		Model:               summaryModel,
		Messages:            messages,
		Tools:               original.Tools,
		Functions:           original.Functions,
		ToolChoice:          json.RawMessage(`"none"`),
		MaxTokens:           &maxTokens,
		ClientSessionID:     original.ClientSessionID,
		TaskID:              "agent_summary",
		NoContextCompaction: true,
		Metadata:            original.Metadata,
		CustomContext:       original.CustomContext,
	}
	summaryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	started := time.Now()
	text, detail, err := executeQoderText(summaryCtx, summaryReq, summaryModel, sa)
	uid := ""
	if sa != nil {
		uid = sa.Account.UID
	}
	if err != nil {
		publishUsage(original.Model, summaryModel, uid, started, usage.Detail{}, true, 0, "context compaction: "+err.Error())
		return "", usage.Detail{}, err
	}
	publishUsage(original.Model, summaryModel, uid, started, detail, false, 0, "")
	return text, detail, nil
}

func executeQoderText(ctx context.Context, req *openAIRequest, modelKey string, sa *storedAuth) (string, usage.Detail, error) {
	body, err := buildQoderBody(req, modelKey, uiUserType(nil), sa)
	if err != nil {
		return "", usage.Detail{}, err
	}
	encodedBody := qoderEncode(body)
	chatURL := chatEndpointFor(sa)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, strings.NewReader(encodedBody))
	if err != nil {
		return "", usage.Detail{}, err
	}
	if err := applyCosyHeaders(httpReq, sa, encodedBody, chatURL, modelKey, true); err != nil {
		return "", usage.Detail{}, err
	}
	stream, status, _, err := openSignedChatStream(httpReq, sa, encodedBody, modelKey)
	if err != nil {
		return "", usage.Detail{}, err
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if status >= 400 {
		payload, _ := io.ReadAll(reader)
		return "", usage.Detail{}, fmt.Errorf("upstream %d: %s", status, truncateRedacted(string(payload), 200))
	}
	completion, err := aggregateQoderSSE(reader, modelKey)
	if err != nil {
		return "", usage.Detail{}, err
	}
	detail := usageDetailFromCompletion(completion)
	text, err := completionText(completion)
	if err != nil {
		return "", detail, err
	}
	summary := parseQoderCompactionSummary(text)
	if strings.TrimSpace(summary) == "" {
		return "", detail, fmt.Errorf("empty compaction summary")
	}
	return summary, detail, nil
}

func completionText(raw []byte) (string, error) {
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return "", err
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("completion has no choices")
	}
	return completion.Choices[0].Message.Content, nil
}

func parseQoderCompactionSummary(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	if start := strings.Index(lower, "<summary>"); start >= 0 {
		start += len("<summary>")
		if endRel := strings.Index(lower[start:], "</summary>"); endRel >= 0 {
			return strings.TrimSpace(text[start : start+endRel])
		}
		return strings.TrimSpace(text[start:])
	}
	reanalysis := regexp.MustCompile(`(?is)<analysis>.*?</analysis>`)
	return strings.TrimSpace(reanalysis.ReplaceAllString(text, ""))
}

func compactionRequestSignature(req *openAIRequest, system []openAIMessage) [32]byte {
	var parts []json.RawMessage
	if req.Instructions != "" {
		raw, _ := json.Marshal(req.Instructions)
		parts = append(parts, raw)
	}
	if len(req.Tools) > 0 {
		parts = append(parts, req.Tools)
	}
	if len(req.Functions) > 0 {
		parts = append(parts, req.Functions)
	}
	raw, _ := json.Marshal(parts)
	systemHash := hashCompactionMessages(system)
	sum := sha256.New()
	_, _ = sum.Write(systemHash[:])
	_, _ = sum.Write(raw)
	var out [32]byte
	copy(out[:], sum.Sum(nil))
	return out
}

func compactionCacheKey(sa *storedAuth, modelKey, clientSession string) string {
	clientSession = strings.TrimSpace(clientSession)
	if clientSession == "" {
		return ""
	}
	region, uid := "", ""
	if sa != nil {
		region, uid = string(sa.region()), sa.Account.UID
	}
	sum := sha256.Sum256([]byte(region + "\x00" + uid + "\x00" + modelKey + "\x00" + clientSession))
	return hex.EncodeToString(sum[:])
}

func lookupCompactionCache(key string, signature [32]byte, prefix []openAIMessage) (*compactionCacheEntry, bool) {
	if key == "" {
		return nil, false
	}
	compactionCache.Lock()
	defer compactionCache.Unlock()
	entry, ok := compactionCache.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.lastUsed) > compactionCacheTTL || entry.signature != signature || entry.covered > len(prefix) {
		delete(compactionCache.entries, key)
		return nil, false
	}
	if hashCompactionMessages(prefix[:entry.covered]) != entry.prefixHash {
		return nil, false
	}
	entry.lastUsed = time.Now()
	copyEntry := *entry
	return &copyEntry, true
}

func cacheCompaction(key string, entry compactionCacheEntry) {
	compactionCache.Lock()
	defer compactionCache.Unlock()
	if len(compactionCache.entries) >= compactionCacheEntries {
		var oldestKey string
		var oldest time.Time
		for key, candidate := range compactionCache.entries {
			if oldest.IsZero() || candidate.lastUsed.Before(oldest) {
				oldestKey, oldest = key, candidate.lastUsed
			}
		}
		delete(compactionCache.entries, oldestKey)
	}
	compactionCache.entries[key] = &entry
}

func acquireCompactionLock(key string) func() {
	if key == "" {
		key = fmt.Sprintf("anonymous-%d", compactionAnonymousLock.Add(1))
	}
	compactionLocks.Lock()
	entry := compactionLocks.entries[key]
	if entry == nil {
		entry = &compactionKeyedLock{}
		compactionLocks.entries[key] = entry
	}
	entry.refs++
	compactionLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		compactionLocks.Lock()
		entry.refs--
		if entry.refs == 0 && compactionLocks.entries[key] == entry {
			delete(compactionLocks.entries, key)
		}
		compactionLocks.Unlock()
	}
}

func hashCompactionMessages(messages []openAIMessage) [32]byte {
	images := make([][]string, len(messages))
	for i := range messages {
		images[i] = messages[i].Images
	}
	raw, _ := json.Marshal(struct {
		Messages []openAIMessage `json:"messages"`
		Images   [][]string      `json:"images"`
	}{messages, images})
	return sha256.Sum256(raw)
}

func estimateOpenAIRequestTokens(req *openAIRequest) int {
	if req == nil {
		return 0
	}
	total := 4
	messages := requestMessages(req)
	for _, message := range messages {
		total += 4 + estimateTextTokens(message.Role) + estimateTextTokens(message.Content)
		total += estimateTextTokens(string(message.ToolCalls))
		total += estimateTextTokens(message.ToolCallID)
		total += estimateTextTokens(message.ReasoningContent)
		total += len(message.Images) * compactionImageTokens
	}
	if len(req.Tools) > 0 {
		total += estimateTextTokens(string(req.Tools))
	}
	if len(req.Functions) > 0 {
		total += estimateTextTokens(string(req.Functions))
	}
	if len(req.ToolChoice) > 0 {
		total += estimateTextTokens(string(req.ToolChoice))
	}
	return total
}

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii, tokens := 0, 0
	flushASCII := func() {
		if ascii > 0 {
			tokens += (ascii + 3) / 4
			ascii = 0
		}
	}
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
			continue
		}
		flushASCII()
		switch {
		case r >= 0x2E80:
			tokens++
		default:
			tokens += 1
		}
	}
	flushASCII()
	return tokens
}
