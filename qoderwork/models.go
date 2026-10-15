// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbModels is the static fallback model list for QoderWork CN. Keys mirror
// /root/qoderwork/models_list.json (KNOWLEDGE §6.2). Aliases use the qoder/
// prefix in AuthAttributes; bare IDs work too. Dynamic refresh via
// /algo/api/v2/model/list replaces this at runtime when an account is present.
func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "auto", Name: "Auto", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel_preview", Name: "Qwen3.8-Max-Preview", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel_latest", Name: "Qwen3.7-Max", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qmodel", Name: "Qwen3.7-Plus", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "q36fmodel", Name: "Qwen3.6-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "dmodel", Name: "DeepSeek-V4-Pro", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "dfmodel", Name: "DeepSeek-V4-Flash", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "gm51model", Name: "GLM-5.2", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kmodel", Name: "Kimi-K2.7-Code", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "mmodel", Name: "MiniMax-M2.7", ContextLength: 180000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

func cachedDynamicModels() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.models) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsCacheTTL {
		return dynamicModelsCache.models, true
	}
	return nil, false
}

func storeDynamicModels(models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = models
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

// modelsFetchStatus is the most recent dynamic-catalog outcome. The management
// API exposes it as /plugins/qoderwork/models/status so "拉取模型异常" can be
// diagnosed without another build.
type modelsFetchStatus struct {
	Source      string   `json:"source"` // dynamic | static
	FetchedAt   string   `json:"fetched_at,omitempty"`
	Count       int      `json:"count"`
	LastError   string   `json:"last_error,omitempty"`
	LastErrorAt string   `json:"last_error_at,omitempty"`
	Attempts    []string `json:"attempts,omitempty"`
}

var (
	modelsStatusMu sync.RWMutex
	modelsStatus   modelsFetchStatus

	// dynamicModelsBackoff prevents a broken catalog endpoint from turning
	// every model.for_auth / model.static call into a burst of upstream
	// requests. Successes are cached for dynamicModelsCacheTTL; failures are
	// suppressed for dynamicModelsFailureTTL unless the management refresh
	// endpoint clears the backoff.
	dynamicModelsBackoffMu  sync.RWMutex
	dynamicModelsBackoffEnd time.Time
	dynamicModelsBackoffErr error
)

const dynamicModelsFailureTTL = 2 * time.Minute

func modelsBackoffActive() (bool, error) {
	dynamicModelsBackoffMu.RLock()
	defer dynamicModelsBackoffMu.RUnlock()
	if dynamicModelsBackoffEnd.IsZero() || time.Now().After(dynamicModelsBackoffEnd) {
		return false, nil
	}
	return true, dynamicModelsBackoffErr
}

func setModelsBackoff(err error) {
	dynamicModelsBackoffMu.Lock()
	dynamicModelsBackoffEnd = time.Now().Add(dynamicModelsFailureTTL)
	dynamicModelsBackoffErr = err
	dynamicModelsBackoffMu.Unlock()
}

func clearModelsBackoff() {
	dynamicModelsBackoffMu.Lock()
	dynamicModelsBackoffEnd = time.Time{}
	dynamicModelsBackoffErr = nil
	dynamicModelsBackoffMu.Unlock()
}

func recordModelsStatus(source string, count int, attempts []string, fetchErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	st := modelsFetchStatus{Source: source, Count: count, Attempts: attempts}
	modelsStatusMu.Lock()
	if source == "dynamic" {
		st.FetchedAt = now
		st.LastError = modelsStatus.LastError
		st.LastErrorAt = modelsStatus.LastErrorAt
	} else {
		st.FetchedAt = modelsStatus.FetchedAt
		if fetchErr != nil {
			st.LastError = fetchErr.Error()
			st.LastErrorAt = now
		} else {
			st.LastError = modelsStatus.LastError
			st.LastErrorAt = modelsStatus.LastErrorAt
		}
	}
	modelsStatus = st
	modelsStatusMu.Unlock()
}

func currentModelsStatus() modelsFetchStatus {
	modelsStatusMu.RLock()
	defer modelsStatusMu.RUnlock()
	out := modelsStatus
	if len(modelsStatus.Attempts) > 0 {
		out.Attempts = append([]string(nil), modelsStatus.Attempts...)
	}
	return out
}

func clearDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
}

func fetchDynamicModels() []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	fallback := wbModels()
	if active, _ := modelsBackoffActive(); active {
		return fallback
	}
	files, err := hostAuthListFiles()
	if err != nil || len(files) == 0 {
		recordModelsStatus("static", len(fallback), nil, err)
		return fallback
	}
	// Strict filename-prefix match — same filter as host_auth.go hostAuthList.
	// (Earlier code also matched files containing "codebuddy" anywhere, which
	// would wrongly include workbuddy-*.json auths here and cause us to call
	// the qoderwork models API with a workbuddy token.)
	prefix := providerName + "-"
	var attempts []string
	var lastErr error
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			attempts = append(attempts, f.Name+": host get: "+truncateRedacted(err.Error(), 120))
			lastErr = err
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			parseErr := err
			if parseErr == nil {
				parseErr = fmt.Errorf("empty stored auth")
			}
			attempts = append(attempts, f.Name+": parse: "+truncateRedacted(parseErr.Error(), 120))
			lastErr = parseErr
			continue
		}
		dyn, err := callModelsAPIWithFallback(sa)
		if err != nil {
			attempts = append(attempts, f.Name+": "+truncateRedacted(err.Error(), 180))
			lastErr = err
			continue
		}
		if len(dyn) > 0 {
			storeDynamicModels(dyn)
			recordModelsStatus("dynamic", len(dyn), attempts, nil)
			return dyn
		}
		lastErr = fmt.Errorf("empty model catalog from %s", f.Name)
	}
	if lastErr != nil {
		setModelsBackoff(lastErr)
	}
	recordModelsStatus("static", len(fallback), attempts, lastErr)
	return fallback
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	if active, _ := modelsBackoffActive(); active {
		return wbModels()
	}
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		return fetchDynamicModels()
	}
	if dyn, err := callModelsAPIWithFallback(sa); err == nil && len(dyn) > 0 {
		storeDynamicModels(dyn)
		recordModelsStatus("dynamic", len(dyn), nil, nil)
		return dyn
	}
	return fetchDynamicModels()
}

// callModelsAPIWithFallback retries the catalog through a PAT-exchanged job
// token when the account is OAuth-only and also carries a PAT. Some QoderWork
// tenants accept the OAuth device token (dt-) for chat but reject it for the
// catalog; the PAT -> jt- path is the same token family the CLI uses for model
// discovery, so it is a safe transient retry (it is never persisted over the
// OAuth credential).
func callModelsAPIWithFallback(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	models, err := callModelsAPI(sa)
	if err == nil && len(models) > 0 {
		return models, nil
	}
	if strings.HasPrefix(sa.Auth.AccessToken, "dt-") && strings.HasPrefix(sa.Auth.PersonalToken, "pt-") {
		tok, xerr := exchangePATForJobToken(sa.Auth.PersonalToken)
		if xerr == nil && tok != nil && tok.Token != "" {
			clone := *sa
			clone.Auth.AccessToken = tok.Token
			if tok.RefreshToken != "" {
				clone.Auth.RefreshToken = tok.RefreshToken
			}
			if m2, err2 := callModelsAPI(&clone); err2 == nil && len(m2) > 0 {
				return m2, nil
			}
		}
	}
	if err == nil {
		err = fmt.Errorf("empty model catalog")
	}
	return nil, err
}

type modelListRequest struct {
	Method   string
	URL      string
	SignBody string
	SendBody []byte
}

// modelListRequests returns the catalog wire variants in descending likelihood.
// The first form is the one the current official clients use: bodyless GET,
// empty-body COSY signature, no ?Encode=1 query. The remaining forms are
// compatibility fallbacks for older/newer gateway builds.
func modelListRequests() []modelListRequest {
	base := strings.TrimSpace(endpointModels)
	if base == "" {
		base = gatewayBaseCN + "/algo/api/v2/model/list"
	}
	encodedEmpty := qoderEncode([]byte("{}"))
	return []modelListRequest{
		{Method: http.MethodGet, URL: base},
		{Method: http.MethodGet, URL: base + "?Encode=1"},
		{Method: http.MethodGet, URL: base + "?Encode=1", SignBody: encodedEmpty},
		{Method: http.MethodPost, URL: base, SignBody: encodedEmpty, SendBody: []byte(encodedEmpty)},
		{Method: http.MethodPost, URL: base + "?Encode=1", SignBody: encodedEmpty, SendBody: []byte(encodedEmpty)},
	}
}

// callModelsAPI fetches /algo/api/v2/model/list with COSY signing. It tries the
// current bodyless wire form first and then the legacy ?Encode=1 / encoded-body
// forms so a gateway-side protocol change cannot silently degrade the dynamic
// list back to the static fallback.
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var lastErr error
	for _, attempt := range modelListRequests() {
		models, err := doModelListRequest(ctx, sa, attempt)
		if err == nil && len(models) > 0 {
			return models, nil
		}
		if err == nil {
			err = fmt.Errorf("empty model catalog")
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("model catalog fetch failed")
	}
	return nil, lastErr
}

func doModelListRequest(ctx context.Context, sa *storedAuth, attempt modelListRequest) ([]pluginapi.ModelInfo, error) {
	var body io.Reader
	if len(attempt.SendBody) > 0 {
		body = bytes.NewReader(attempt.SendBody)
	}
	req, err := http.NewRequestWithContext(ctx, attempt.Method, attempt.URL, body)
	if err != nil {
		return nil, err
	}
	sess, err := cosySessionFor(sa)
	if err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	hdr, err := sess.modelListHeaders(sa.Account.UID, attempt.URL)
	if err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	if len(attempt.SendBody) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", attempt.Method, attempt.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s status %d: %s", attempt.Method, attempt.URL, resp.StatusCode, truncateRedacted(string(resp.Body), 180))
	}
	return parseModelListResponse(resp.Body)
}

type modelCatalogEntry struct {
	Key             string `json:"key"`
	DisplayName     string `json:"display_name"`
	Enable          *bool  `json:"enable"`
	MaxInputTokens  int64  `json:"max_input_tokens"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	ContextConfig   map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	} `json:"context_config"`
}

func (m modelCatalogEntry) enabled() bool {
	return m.Enable == nil || *m.Enable
}

func (m modelCatalogEntry) contextLength() int64 {
	var listed int64
	for _, c := range m.ContextConfig {
		if c.TokenCount <= 0 {
			continue
		}
		if c.IsDefault {
			return c.TokenCount
		}
		if c.TokenCount > listed {
			listed = c.TokenCount
		}
	}
	if m.MaxInputTokens > 0 {
		return m.MaxInputTokens
	}
	if listed > 0 {
		return listed
	}
	return 180000
}

func (m modelCatalogEntry) maxCompletion() int64 {
	if m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	return 8192
}

// parseModelListResponse accepts plain JSON, QoderEncoding-encoded JSON and a
// {code,data:{...}} envelope. The catalog scenes are read in preference order
// (chat first, inline second), and duplicate keys are collapsed.
func parseModelListResponse(body []byte) ([]pluginapi.ModelInfo, error) {
	raw := body
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		decoded, derr := qoderDecode(raw)
		if derr != nil {
			return nil, fmt.Errorf("models parse: %w", err)
		}
		raw = decoded
		if err2 := json.Unmarshal(raw, &root); err2 != nil {
			return nil, fmt.Errorf("models parse (decoded): %w", err2)
		}
	}
	if _, ok := root["chat"]; !ok {
		if dataRaw, ok := root["data"]; ok {
			var dataMap map[string]json.RawMessage
			if json.Unmarshal(dataRaw, &dataMap) == nil {
				root = dataMap
			}
		}
	}
	var out []pluginapi.ModelInfo
	seen := make(map[string]struct{})
	for _, scene := range []string{"chat", "inline"} {
		sceneRaw, ok := root[scene]
		if !ok {
			continue
		}
		var entries []modelCatalogEntry
		if err := json.Unmarshal(sceneRaw, &entries); err != nil {
			continue
		}
		for _, m := range entries {
			if !m.enabled() {
				continue
			}
			key := strings.TrimSpace(m.Key)
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			name := strings.TrimSpace(m.DisplayName)
			if name == "" {
				name = key
			}
			out = append(out, pluginapi.ModelInfo{
				ID:                         key,
				Name:                       name,
				ContextLength:              m.contextLength(),
				MaxCompletionTokens:        m.maxCompletion(),
				OwnedBy:                    providerName,
				SupportedGenerationMethods: []string{"chat"},
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled chat models in response")
	}
	return out, nil
}

// handleModelsStatus returns the last dynamic-model fetch outcome. It is
// read-only and intentionally exposes upstream error text (truncated) so the
// panel/management caller can see why the provider fell back to static models.
func handleModelsStatus() map[string]any {
	st := currentModelsStatus()
	out := map[string]any{
		"status": st,
		"static": len(wbModels()),
	}
	if models, ok := cachedDynamicModels(); ok {
		out["cached"] = len(models)
	}
	return out
}

// handleModelsRefresh clears the 5-minute cache and refetches the dynamic
// catalog immediately. The static fallback is returned when every auth fails;
// modelsStatus still records the per-account error.
func handleModelsRefresh() map[string]any {
	clearDynamicModelsCache()
	clearModelsBackoff()
	models := fetchDynamicModels()
	return map[string]any{
		"count":  len(models),
		"status": currentModelsStatus(),
	}
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the qoderwork provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := fetchDynamicModels()
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
