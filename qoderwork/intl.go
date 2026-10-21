// intl.go implements the QoderWork international realm (qoder.com): a separate
// service from CN, reached with the same device-authorization grant but a
// different inference protocol.
//
// The international gateway (api2-v2.qoder.sh) speaks the OpenAI Chat
// Completions wire format directly and authenticates with a plain
// `Authorization: Bearer {jt-}` job token — there is no COSY signing and no
// agent_chat_generation envelope. cockpit-tools
// (github.com/jlcodes99/cockpit-tools) drives the international login and
// account/quota surfaces through these same hosts.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// intlChatPath is the international OpenAI-compatible chat endpoint.
	intlChatPath = "/model/v1/chat/completions"
	// intlModelListPath is the international model-catalog endpoint. Its exact
	// path is not yet confirmed against a live international account (the
	// gateway answers 404 for every unauthenticated probe), so the catalog
	// tries several candidates and falls back to intlStaticModels on failure.
	intlModelListPath = "/model/v1/model/list"

	// intlCosyVersion is the client version presented in Cosy-Version headers.
	// It must match an official client build the gateway recognises.
	intlCosyVersion = "1.0.0"

	intlDefaultContext = 200000
	intlDefaultMaxOut  = 32000
)

// intlStaticModels is the fallback catalog. Keys are the international wire
// model ids (the gateway accepts them verbatim in `model`).
func intlStaticModels() []pluginapi.ModelInfo {
	defs := []struct {
		id   string
		name string
	}{
		{"auto", "Auto"},
		{"claude-sonnet-4.5", "Claude Sonnet 4.5"},
		{"claude-opus-4.1", "Claude Opus 4.1"},
		{"gpt-5", "GPT-5"},
		{"gemini-2.5-pro", "Gemini 2.5 Pro"},
		{"qwen3-max", "Qwen3 Max"},
		{"kimi-k2", "Kimi K2"},
	}
	out := make([]pluginapi.ModelInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, pluginapi.ModelInfo{
			ID:                         d.id,
			Name:                       d.name,
			ContextLength:              intlDefaultContext,
			MaxCompletionTokens:        intlDefaultMaxOut,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out
}

// intlUpstreamModel maps a CPA-facing model name onto the international wire id.
// Unknown names pass through unchanged; the gateway rejects genuinely unknown
// ids, which is preferable to silently routing to auto.
func intlUpstreamModel(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return "auto"
	}
	switch strings.ToLower(m) {
	case "qoder-auto", "auto":
		return "auto"
	case "claude-sonnet-4.5", "claude-sonnet-4-5", "sonnet":
		return "claude-sonnet-4.5"
	case "claude-opus-4.1", "claude-opus-4-1", "opus":
		return "claude-opus-4.1"
	case "gpt-5", "gpt5":
		return "gpt-5"
	case "gemini-2.5-pro", "gemini-2.5", "gemini":
		return "gemini-2.5-pro"
	case "qwen3-max", "qwen3", "qwen":
		return "qwen3-max"
	case "kimi-k2", "kimi":
		return "kimi-k2"
	}
	return m
}

// intlHeaders builds the request headers for one international gateway call:
// a plain Bearer job token plus the client-identity headers the official
// international client sends. COSY signing is not used on this realm.
func intlHeaders(sa *storedAuth) map[string]string {
	h := map[string]string{
		"Authorization":    "Bearer " + sa.Auth.AccessToken,
		"Content-Type":     "application/json",
		"Accept":           "application/json",
		"User-Agent":       "qoder/" + intlCosyVersion,
		"Cosy-Version":     intlCosyVersion,
		"Cosy-Clienttype":  "5",
		"Cosy-Data-Policy": "disagree",
	}
	machineID := strings.TrimSpace(sa.Auth.MachineID)
	if machineID == "" {
		machineID = stableMachineID(sa)
	}
	if official := readOfficialMachineIdentity(); !official.empty() {
		if official.CosyVersion != "" {
			h["Cosy-Version"] = official.CosyVersion
		}
		if official.MachineType != "" {
			h["Cosy-Machinetype"] = official.MachineType
		}
		if official.MachineOS != "" {
			h["Cosy-Machineos"] = official.MachineOS
		}
		if official.MachineToken != "" {
			h["Cosy-Machinetoken"] = official.MachineToken
		}
	}
	if _, ok := h["Cosy-Machinetype"]; !ok {
		h["Cosy-Machinetype"] = "5"
	}
	if _, ok := h["Cosy-Machineos"]; !ok {
		h["Cosy-Machineos"] = buildMachineOS()
	}
	if _, ok := h["Cosy-Machinetoken"]; !ok {
		h["Cosy-Machinetoken"] = machineID
	}
	h["Cosy-Machineid"] = machineID
	return h
}

// buildIntlChatBody rewrites an inbound OpenAI chat request for the
// international gateway: the wire model id is applied and the response is
// always requested as a stream (the executor folds or relays it). Every other
// field (messages, tools, temperature, …) is forwarded untouched.
func buildIntlChatBody(payload []byte, upstreamModel string) ([]byte, error) {
	body := map[string]any{}
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, fmt.Errorf("payload parse: %w", err)
		}
	}
	body["model"] = upstreamModel
	body["stream"] = true
	return json.Marshal(body)
}

// executeIntl runs one non-streaming international chat request by streaming
// upstream and folding the SSE into a chat.completion.
func executeIntl(req pluginapi.ExecutorRequest, sa *storedAuth, upstreamModel string, started time.Time, authUID string) ([]byte, error) {
	body, err := buildIntlChatBody(req.Payload, upstreamModel)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	url := sa.endpoints().Gateway + intlChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range intlHeaders(sa) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		payload, _ := io.ReadAll(reader)
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(payload))
		reconcileAfterExecutorError(req.AuthID, statusCode, string(payload))
		return nil, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(payload), 200))
	}
	completion, err := aggregateCompletion(reader, req.Model)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, err
	}
	publishUsage(req.Model, upstreamModel, authUID, started, usageDetailFromCompletion(completion), false, 0, "")
	invalidateAccountCredits(req.AuthID, authUID)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// collectIntlStream is the synchronous stream fallback: relay each upstream
// OpenAI SSE chunk to the client.
func collectIntlStream(sa *storedAuth, payload []byte, upstreamModel string, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, int, error) {
	body, err := buildIntlChatBody(payload, upstreamModel)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	url := sa.endpoints().Gateway + intlChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range intlHeaders(sa) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	if statusCode >= 400 {
		p, _ := io.ReadAll(newHostStreamReader(stream))
		return nil, statusCode, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(p), 200))
	}
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if collector != nil {
			collector.feed(data)
		}
		cleaned := cleanChunkJSON(data)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: json.RawMessage(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, 0, fmt.Errorf("upstream stream read error: %w", err)
	}
	return chunks, 0, nil
}

// pumpIntlStream streams the international response asynchronously via the
// host stream bridge.
func pumpIntlStream(sa *storedAuth, payload []byte, upstreamModel, streamID string, sseFramed bool, requestedModel, authUID string, started time.Time, authID string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, err := buildIntlChatBody(payload, upstreamModel)
	if err != nil {
		streamEmitError(streamID, "body build: "+err.Error())
		streamClose(streamID)
		return
	}
	url := sa.endpoints().Gateway + intlChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		streamEmitError(streamID, err.Error())
		streamClose(streamID)
		return
	}
	for k, v := range intlHeaders(sa) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		streamEmitError(streamID, "http_error: "+err.Error())
		streamClose(streamID)
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		p, _ := io.ReadAll(newHostStreamReader(stream))
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(p), 200)))
		streamClose(streamID)
		return
	}
	collector := &sseUsageCollector{}
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		collector.feed(data)
		cleaned := cleanChunkJSON(data)
		if cleaned == "" {
			continue
		}
		out := cleaned
		if sseFramed {
			out = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(out)); err != nil {
			break
		}
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "")
	invalidateAccountCredits(authID, authUID)
	streamClose(streamID)
}

// handleExecStreamIntl serves the international realm's stream RPC. It mirrors
// handleExecStream's shape (sync chunk collection, or an async pump) but sends
// the caller's OpenAI payload straight to the gateway.
func handleExecStreamIntl(req executorStreamRequest, sa *storedAuth, started time.Time, authUID string) ([]byte, error) {
	upstreamModel := intlUpstreamModel(stripProviderPrefix(req.Model))
	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)
	bodyRaw := req.Payload
	if len(bodyRaw) == 0 {
		bodyRaw = req.OriginalRequest
	}

	if req.StreamID == "" {
		collector := &sseUsageCollector{}
		chunks, statusCode, errCollect := collectIntlStream(sa, bodyRaw, upstreamModel, sseFramed, collector)
		if errCollect != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, errCollect.Error())
			return nil, errCollect
		}
		publishUsage(req.Model, upstreamModel, authUID, started, collector.detail(), false, 0, "")
		invalidateAccountCredits(req.AuthID, authUID)
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	go pumpIntlStream(sa, bodyRaw, upstreamModel, req.StreamID, sseFramed, req.Model, authUID, started, req.AuthID)
	return okEnvelope(streamResponse{Headers: headers})
}

// qoderIntlCatalogGateway is the international gateway that serves the model
// catalog. It is not the chat gateway: CLI 1.1.69 sends catalog requests to
// api2.qoder.sh/algo/api/v2/model/list (COSY-signed), while chat uses
// api2-v2.qoder.sh/model/v1/chat/completions.
const qoderIntlCatalogGateway = "https://api2.qoder.sh"

// intlCatalogGatewayOverride lets tests point the international catalog at a
// local server. Empty in production.
var intlCatalogGatewayOverride = ""

func intlCatalogBase(sa *storedAuth) string {
	if intlCatalogGatewayOverride != "" {
		return intlCatalogGatewayOverride + "/algo/api/v2/model/list"
	}
	return qoderIntlCatalogGateway + "/algo/api/v2/model/list"
}

// callIntlModelsAPI fetches the international catalog. Qoder CLI 1.1.69 uses
// the same COSY-signed scene catalog for both realms, so that is tried first;
// the Bearer /model/v1/models candidates remain as a fallback for other
// gateway builds. A failure here returns an error and the caller falls back to
// the static international list — discovery must never break chat.
func callIntlModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	if models, err := callModelsAPIWithBase(sa, intlCatalogBase(sa)); err == nil && len(models) > 0 {
		return models, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var lastErr error
	for _, path := range intlModelListPaths {
		models, err := doIntlModelList(ctx, sa, path)
		if err == nil && len(models) > 0 {
			return models, nil
		}
		if err == nil {
			err = fmt.Errorf("empty intl catalog at %s", path)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("intl model catalog fetch failed")
	}
	return nil, lastErr
}

// intlModelListPaths are the candidate catalog paths, tried in order.
var intlModelListPaths = []string{
	"/model/v1/models",
	intlModelListPath,
	"/model/v1/model/list",
}

func doIntlModelList(ctx context.Context, sa *storedAuth, path string) ([]pluginapi.ModelInfo, error) {
	url := sa.endpoints().Gateway + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range intlHeaders(sa) {
		req.Header.Set(k, v)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("intl models %s status %d: %s", path, resp.StatusCode, truncateRedacted(string(resp.Body), 160))
	}
	return parseIntlModelList(resp.Body)
}

// parseIntlModelList accepts the OpenAI /v1/models shape ({"data":[{id,...}]})
// and the CN-style scene map, so either gateway build resolves.
func parseIntlModelList(body []byte) ([]pluginapi.ModelInfo, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("intl models parse: %w", err)
	}
	// OpenAI shape: {"data":[{id, ...}]}
	if dataRaw, ok := root["data"]; ok {
		var entries []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			OwnedBy string `json:"owned_by"`
		}
		if err := json.Unmarshal(dataRaw, &entries); err == nil && len(entries) > 0 {
			out := make([]pluginapi.ModelInfo, 0, len(entries))
			for _, e := range entries {
				id := strings.TrimSpace(e.ID)
				if id == "" {
					continue
				}
				name := strings.TrimSpace(e.Name)
				if name == "" {
					name = id
				}
				out = append(out, pluginapi.ModelInfo{
					ID:                         id,
					Name:                       name,
					ContextLength:              intlDefaultContext,
					MaxCompletionTokens:        intlDefaultMaxOut,
					OwnedBy:                    providerName,
					SupportedGenerationMethods: []string{"chat"},
				})
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	// Scene-map shape: reuse the CN parser.
	return parseModelListResponse(body)
}
