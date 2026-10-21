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

// modelCacheIdentity namespaces the catalog cache per credential owner.
//
// Without it, the first account that succeeds populates a process-wide cache
// and every other QoderWork auth is shown that account's catalog — which is
// exactly why a multi-account setup could advertise a stale, wrong model set.
// An empty owner falls back to the global namespace used by model.static.
func modelCacheIdentity(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	if uid := strings.TrimSpace(sa.Account.UID); uid != "" {
		return sa.region().String() + ":" + uid
	}
	if pt := strings.TrimSpace(sa.Auth.PersonalToken); pt != "" {
		return sa.region().String() + ":pt:" + pt
	}
	return sa.region().String() + ":token:" + strings.TrimSpace(sa.Auth.AccessToken)
}

func cachedDynamicModelsFor(owner string) ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	e := dynamicModelsCache.entries[owner]
	if e != nil && len(e.models) > 0 && time.Since(e.fetched) < dynamicModelsCacheTTL {
		return e.models, true
	}
	return nil, false
}

func storeDynamicModelsFor(owner string, models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	if dynamicModelsCache.entries == nil {
		dynamicModelsCache.entries = make(map[string]*dynamicModelsCacheEntry)
	}
	dynamicModelsCache.entries[owner] = &dynamicModelsCacheEntry{models: models, fetched: time.Now()}
	dynamicModelsCache.Unlock()
}

// mostRecentDynamicModels returns the freshest cached catalog across owners.
// It backs model.static, whose request carries no credential to key on.
func mostRecentDynamicModels() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	var best *dynamicModelsCacheEntry
	for _, e := range dynamicModelsCache.entries {
		if e == nil || len(e.models) == 0 || time.Since(e.fetched) >= dynamicModelsCacheTTL {
			continue
		}
		if best == nil || e.fetched.After(best.fetched) {
			best = e
		}
	}
	if best == nil {
		return nil, false
	}
	return best.models, true
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
	dynamicModelsCache.entries = nil
	dynamicModelsCache.Unlock()
}

func fetchDynamicModels() []pluginapi.ModelInfo {
	if models, ok := mostRecentDynamicModels(); ok {
		return models
	}
	fallback := wbModels()
	sawIntl := false
	if active, _ := modelsBackoffActive(); active {
		if modelsAnyIntlAuth() {
			return intlStaticModels()
		}
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
		if sa.region() == regionIntl {
			sawIntl = true
		}
		dyn, err := callModelsForAuth(sa)
		if err != nil {
			attempts = append(attempts, f.Name+": "+truncateRedacted(err.Error(), 180))
			lastErr = err
			continue
		}
		if len(dyn) > 0 {
			storeDynamicModelsFor(modelCacheIdentity(sa), dyn)
			recordModelsStatus("dynamic", len(dyn), attempts, nil)
			return dyn
		}
		lastErr = fmt.Errorf("empty model catalog from %s", f.Name)
	}
	if lastErr != nil {
		setModelsBackoff(lastErr)
	}
	if sawIntl {
		// At least one account is international; present its static list rather
		// than the CN keys, which the international gateway would reject. When
		// both realms are present we union the two so neither region's models go
		// missing on the global /v1/models listing. Per-account routing still
		// uses model.for_auth, which is always realm-exact.
		sawCN := modelsAnyCNAuth()
		if sawCN {
			fallback = unionModels(wbModels(), intlStaticModels())
		} else {
			fallback = intlStaticModels()
		}
	}
	recordModelsStatus("static", len(fallback), attempts, lastErr)
	return fallback
}

// modelsAnyIntlAuth reports whether any qoderwork auth belongs to the
// international realm. Used only for the global static fallback.
func modelsAnyIntlAuth() bool { return anyAuthRegion(regionIntl) }

// modelsAnyCNAuth reports whether any qoderwork auth belongs to the CN realm.
func modelsAnyCNAuth() bool { return anyAuthRegion(regionCN) }

func anyAuthRegion(region qoderRegion) bool {
	files, err := hostAuthListFiles()
	if err != nil {
		return false
	}
	prefix := providerName + "-"
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			continue
		}
		if sa.region() == region {
			return true
		}
	}
	return false
}

// unionModels concatenates two catalogs, keeping the first occurrence of each
// model id.
func unionModels(a, b []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]pluginapi.ModelInfo, 0, len(a)+len(b))
	for _, list := range [][]pluginapi.ModelInfo{a, b} {
		for _, m := range list {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		return fetchDynamicModels()
	}
	owner := modelCacheIdentity(sa)
	if models, ok := cachedDynamicModelsFor(owner); ok {
		return models
	}
	if active, _ := modelsBackoffActive(); active {
		if sa.region() == regionIntl {
			if anyAuthRegion(regionCN) {
				return unionModels(wbModels(), intlStaticModels())
			}
			return intlStaticModels()
		}
		return wbModels()
	}
	if dyn, err := callModelsForAuth(sa); err == nil && len(dyn) > 0 {
		storeDynamicModelsFor(owner, dyn)
		recordModelsStatus("dynamic", len(dyn), nil, nil)
		return dyn
	}
	return fetchDynamicModels()
}

// callModelsForAuth dispatches catalog discovery by realm: CN uses the
// COSY-signed gateway endpoint, international uses the OpenAI-style gateway
// with a Bearer job token.
func callModelsForAuth(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	if sa.region() == regionIntl {
		return callIntlModelsAPI(sa)
	}
	return callModelsAPIWithFallback(sa)
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
//
// The first form is the current Qoder CLI/Worker 1.1.69 wire shape:
// `GET /algo/api/v2/model/list?Encode=1` with an empty-body COSY signature.
// The bare `/api/v2/model/list` and signature-body POST forms are kept as
// compatibility fallbacks for other gateway builds; discovery must never
// silently fall back to the static catalog because one variant changed.
func modelListRequests() []modelListRequest { return modelListRequestsForBase(endpointModels) }

func modelListRequestsForBase(baseURL string) []modelListRequest {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = gatewayBaseCN + "/algo/api/v2/model/list"
	}
	bareBase := base
	if strings.Contains(base, "/algo/api/") {
		bareBase = strings.Replace(base, "/algo/api/", "/api/", 1)
	}
	// The catalog is GET-only: the gateway answers 400 "Request method 'POST'
	// not supported". A trailing POST fallback used to overwrite the successful
	// GET result in models/status, reporting "static" even when discovery had
	// worked, so only GET variants are attempted here.
	return []modelListRequest{
		{Method: http.MethodGet, URL: base + "?Encode=1"},
		{Method: http.MethodGet, URL: base},
		{Method: http.MethodGet, URL: bareBase + "?Encode=1"},
		{Method: http.MethodGet, URL: bareBase},
	}
}

// callModelsAPI fetches /algo/api/v2/model/list with COSY signing. It tries the
// current bodyless wire form first and then the legacy ?Encode=1 / encoded-body
// forms so a gateway-side protocol change cannot silently degrade the dynamic
// list back to the static fallback.
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	return callModelsAPIWithBase(sa, endpointModels)
}

// callModelsAPIWithBase fetches a COSY-signed catalog from an explicit base
// URL. Both realms use the same catalog protocol; only the gateway host
// differs (CN gateway.qoder.com.cn, INTL api2.qoder.sh).
func callModelsAPIWithBase(sa *storedAuth, base string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var lastErr error
	for _, attempt := range modelListRequestsForBase(base) {
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
	Key             string          `json:"key"`
	ModelKey        string          `json:"model_key"`
	DisplayName     string          `json:"display_name"`
	Format          string          `json:"format"`
	Enable          json.RawMessage `json:"enable"`
	MaxInputTokens  json.Number     `json:"max_input_tokens"`
	MaxOutputTokens json.Number     `json:"max_output_tokens"`
	IsDefault       json.RawMessage `json:"is_default"`
	IsVL            json.RawMessage `json:"is_vl"`
	IsReasoning     json.RawMessage `json:"is_reasoning"`
	ContextConfig   map[string]struct {
		TokenCount json.Number     `json:"token_count"`
		IsDefault  json.RawMessage `json:"is_default"`
		Window     string          `json:"window"`
	} `json:"context_config"`
	DefaultContextWindow json.Number     `json:"default_context_window"`
	AvailableContexts    []json.Number   `json:"available_context_windows"`
	Efforts              []string        `json:"efforts"`
	DefaultEffort        string          `json:"default_effort"`
	SupportsDisabled     bool            `json:"supports_disabled"`
	ServerModel          json.RawMessage `json:"server_model"`
}

// rawBool interprets the mixed boolean encodings the model catalog has used
// across gateway builds (true/false, 1/0, "1"/"0"). A missing value keeps the
// caller's default.
func rawBool(raw json.RawMessage, def bool) bool {
	v := strings.TrimSpace(string(raw))
	if v == "" || v == "null" {
		return def
	}
	switch v {
	case "true", "1", `"1"`, `"true"`, `"TRUE"`:
		return true
	case "false", "0", `"0"`, `"false"`, `"FALSE"`:
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	return def
}

func (m modelCatalogEntry) enabled() bool { return rawBool(m.Enable, true) }

// vision reports whether the upstream catalog marks this model as
// image-capable (is_vl). Advertising it through SupportedInputModalities lets
// clients keep image attachments enabled for vision models and hide them for
// text-only ones.
func (m modelCatalogEntry) vision() bool { return rawBool(m.IsVL, false) }

func (m modelCatalogEntry) key() string {
	if k := strings.TrimSpace(m.Key); k != "" {
		return k
	}
	return strings.TrimSpace(m.ModelKey)
}

func (m modelCatalogEntry) contextLength() int64 {
	var listed int64
	for _, c := range m.ContextConfig {
		n, err := c.TokenCount.Int64()
		if err != nil || n <= 0 {
			continue
		}
		if rawBool(c.IsDefault, false) {
			return n
		}
		if n > listed {
			listed = n
		}
	}
	if n, err := m.DefaultContextWindow.Int64(); err == nil && n > 0 {
		return n
	}
	if n, err := m.MaxInputTokens.Int64(); err == nil && n > 0 {
		return n
	}
	for _, n := range m.AvailableContexts {
		if v, err := n.Int64(); err == nil && v > listed {
			listed = v
		}
	}
	if listed > 0 {
		return listed
	}
	return 180000
}

func (m modelCatalogEntry) maxCompletion() int64 {
	if n, err := m.MaxOutputTokens.Int64(); err == nil && n > 0 {
		return n
	}
	return 8192
}

// modelCatalogScenes is the order in which catalog scenes are read. The Qoder
// desktop/CLI client defaults to the "assistant" scene (CLI 1.1.69, scene
// name from its default brand config); older builds used "chat"/"inline".
var modelCatalogScenes = []string{"assistant", "chat", "inline"}

// modelCatalogScene returns the catalog root. The response is normally a
// scene map ({"assistant":[...],"chat":[...]}), but older builds wrapped it in
// {"data":{...}} or returned a bare array for a single scene.
//
// A gateway response may also be QoderEncoding-encoded, so decode first when
// the raw body is not JSON.
func modelCatalogRoot(body []byte) (map[string]json.RawMessage, error) {
	raw := bytes.TrimSpace(body)
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
	if dataRaw, ok := root["data"]; ok {
		var dataMap map[string]json.RawMessage
		if json.Unmarshal(dataRaw, &dataMap) == nil && hasCatalogScene(dataMap) {
			return dataMap, nil
		}
	}
	return root, nil
}

func hasCatalogScene(root map[string]json.RawMessage) bool {
	for _, scene := range modelCatalogScenes {
		if _, ok := root[scene]; ok {
			return true
		}
	}
	return false
}

// parseModelListResponse accepts plain JSON, QoderEncoding-encoded JSON and a
// {code,data:{...}} envelope. Scenes are read in modelCatalogScenes order and
// duplicate model keys are collapsed, so a model present in both
// "assistant" and "chat" is only advertised once.
func parseModelListResponse(body []byte) ([]pluginapi.ModelInfo, error) {
	root, err := modelCatalogRoot(body)
	if err != nil {
		return nil, err
	}
	var out []pluginapi.ModelInfo
	seen := make(map[string]struct{})
	for _, scene := range modelCatalogScenes {
		sceneRaw, ok := root[scene]
		if !ok {
			continue
		}
		var raws []json.RawMessage
		if err := json.Unmarshal(sceneRaw, &raws); err != nil {
			continue
		}
		for _, rawEntry := range raws {
			// Decode each entry independently: one malformed or
			// differently-typed record must not discard the whole scene.
			var m modelCatalogEntry
			if err := json.Unmarshal(rawEntry, &m); err != nil {
				continue
			}
			if !m.enabled() {
				continue
			}
			key := m.key()
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
			// format identifies the wire protocol the model speaks (openai,
			// anthropic, ...), not its modality. CPA talks chat-completions
			// to every QoderWork model, so it is advertised for all of them.
			modalities := []string{"text"}
			if m.vision() {
				modalities = append(modalities, "image")
			}
			out = append(out, pluginapi.ModelInfo{
				ID:                         key,
				Name:                       name,
				ContextLength:              m.contextLength(),
				MaxCompletionTokens:        m.maxCompletion(),
				OwnedBy:                    providerName,
				SupportedGenerationMethods: []string{"chat"},
				SupportedInputModalities:   modalities,
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
	if models, ok := mostRecentDynamicModels(); ok {
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
	if !ok {
		// Some clients send "qoderwork/alias" or "qoder/alias"; try the bare
		// alias once before giving up.
		for _, prefix := range []string{providerName + "/", "qoder/"} {
			if strings.HasPrefix(key, prefix) {
				if n, hit := modelAliasCache.byAlias[strings.TrimPrefix(key, prefix)]; hit {
					name, ok = n, true
					break
				}
			}
		}
	}
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
