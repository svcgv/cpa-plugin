package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Public IDs include the provider namespace so a Qoder model does not collide
// with another plugin advertising the same display name in the host registry.
func publicModelID(name, upstreamKey string) string {
	slug := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(name)), "-"))
	if slug == "" {
		slug = strings.ToLower(strings.TrimSpace(upstreamKey))
	}
	if slug == "" {
		return ""
	}
	return "qoder/" + slug
}

func publicModelCatalog(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	seen := make(map[string]bool, len(models))
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := publicModelID(model.Name, model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		model.ID = id
		if model.DisplayName == "" {
			model.DisplayName = model.Name
		}
		out = append(out, model)
	}
	return out
}

func resolveRequestModel(sa *storedAuth, model string, attributes map[string]string) (string, error) {
	key := resolveUpstreamModel(stripProviderPrefix(model), attributes)
	if key == "" {
		return "", fmt.Errorf("model is required")
	}
	if entry, ok := metadataForModel(sa, key); ok {
		return entry.key(), nil
	}
	raw, _ := json.Marshal(sa)
	models := fetchDynamicModelsFromStorage(raw)
	if entry, ok := metadataForModel(sa, key); ok {
		return entry.key(), nil
	}
	for _, candidate := range models {
		if strings.EqualFold(candidate.ID, key) || strings.EqualFold(publicModelID(candidate.Name, candidate.ID), model) || strings.EqualFold(strings.TrimPrefix(publicModelID(candidate.Name, candidate.ID), "qoder/"), key) {
			return candidate.ID, nil
		}
	}
	return "", fmt.Errorf("模型 %q 不在该账号的目录中", key)
}

var contextWindows = struct {
	sync.RWMutex
	cn, intl bool
}{cn: true, intl: true}
var catalogMetadata sync.Map // account identity -> map[upstream key]modelCatalogEntry

func setContextWindows(cn, intl bool) {
	contextWindows.Lock()
	changed := contextWindows.cn != cn || contextWindows.intl != intl
	contextWindows.cn, contextWindows.intl = cn, intl
	contextWindows.Unlock()
	if changed {
		clearDynamicModelsCache()
	}
}
func useMaxContext(region qoderRegion) bool {
	contextWindows.RLock()
	defer contextWindows.RUnlock()
	if region == regionIntl {
		return contextWindows.intl
	}
	return contextWindows.cn
}

func storeCatalogMetadata(sa *storedAuth, body []byte) {
	root, err := modelCatalogRoot(body)
	if err != nil {
		return
	}
	entries := map[string]modelCatalogEntry{}
	var discovered []modelCatalogEntry
	for _, scene := range modelCatalogScenes {
		var list []json.RawMessage
		if json.Unmarshal(root[scene], &list) != nil {
			continue
		}
		for _, raw := range list {
			var entry modelCatalogEntry
			if json.Unmarshal(raw, &entry) != nil || !entry.enabled() || entry.key() == "" {
				continue
			}
			discovered = append(discovered, entry)
		}
	}
	// Public names win over short upstream keys on a collision. Scene order
	// remains deterministic and duplicate public names keep the first model.
	for _, entry := range discovered {
		name := strings.TrimPrefix(publicModelID(entry.DisplayName, entry.key()), "qoder/")
		if _, exists := entries[name]; !exists {
			entries[name] = entry
		}
	}
	for _, entry := range discovered {
		key := strings.ToLower(entry.key())
		if _, exists := entries[key]; !exists {
			entries[key] = entry
		}
	}
	catalogMetadata.Store(modelCacheIdentity(sa), entries)
}

func metadataForModel(sa *storedAuth, model string) (modelCatalogEntry, bool) {
	value, ok := catalogMetadata.Load(modelCacheIdentity(sa))
	if !ok {
		return modelCatalogEntry{}, false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "qoder/")
	model = strings.TrimPrefix(model, "qoderwork/")
	entry, ok := value.(map[string]modelCatalogEntry)[model]
	return entry, ok
}

func (m modelCatalogEntry) maxContextLength() int64 {
	n := m.contextLength()
	if v, err := m.MaxInputTokens.Int64(); err == nil && v > n {
		n = v
	}
	for _, option := range m.ContextConfig {
		if v, err := option.TokenCount.Int64(); err == nil && v > n {
			n = v
		}
	}
	for _, option := range m.AvailableContexts {
		if v, err := option.Int64(); err == nil && v > n {
			n = v
		}
	}
	return n
}

func applyModelMetadata(base map[string]any, sa *storedAuth, key string) {
	m, ok := metadataForModel(sa, key)
	if !ok {
		return
	}
	mc, ok := base["model_config"].(map[string]any)
	if !ok {
		return
	}
	if m.Source != "" {
		mc["source"] = m.Source
	}
	reasoning := rawBool(m.IsReasoning, false)
	enabled, enabledOK := m.ThinkingConfig["enabled"]
	disabled, disabledOK := m.ThinkingConfig["disabled"]
	if enabledOK && enabled.IsDefault && !(disabledOK && disabled.IsDefault) {
		reasoning = true
	}
	if disabledOK && disabled.IsDefault && !(enabledOK && enabled.IsDefault) {
		reasoning = false
	}
	mc["is_reasoning"] = reasoning
	if len(m.ContextConfig) == 0 {
		return
	}
	config := map[string]any{}
	defaultCount := 0
	for key, c := range m.ContextConfig {
		count, err := c.TokenCount.Int64()
		if err != nil || count <= 0 {
			continue
		}
		isDefault := rawBool(c.IsDefault, false)
		if isDefault {
			defaultCount++
		}
		config[key] = map[string]any{"token_count": count, "is_default": isDefault}
	}
	if len(config) > 0 && defaultCount == 1 {
		mc["context_config"] = config
	}
}

// Adapt CC Switch effort to an advertised level without rejecting requests.
func resolveReasoning(sa *storedAuth, model, effort string) (string, *bool, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return "", nil, nil
	}
	m, ok := metadataForModel(sa, model)
	if !ok {
		return "", nil, nil
	}
	if effort == "off" || effort == "none" {
		if _, ok := m.ThinkingConfig["disabled"]; ok || m.SupportsDisabled {
			v := false
			return "", &v, nil
		}
		return "", nil, nil
	}
	ranks := map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}
	want, ok := ranks[effort]
	if !ok {
		return "", nil, nil
	}
	candidates := map[string]bool{}
	if enabled, ok := m.ThinkingConfig["enabled"]; ok {
		for name := range enabled.Efforts {
			candidates[strings.ToLower(name)] = true
		}
	}
	for _, name := range m.Efforts {
		candidates[strings.ToLower(name)] = true
	}
	best, bestDistance, bestRank := "", int(^uint(0)>>1), -1
	for name := range candidates {
		value, ok := ranks[name]
		if !ok {
			continue
		}
		distance := value - want
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance || (distance == bestDistance && value > bestRank) {
			best, bestDistance, bestRank = name, distance, value
		}
	}
	if best != "" {
		enabled := true
		return best, &enabled, nil
	}
	if _, ok := m.ThinkingConfig["enabled"]; ok {
		enabled := true
		return "", &enabled, nil
	}
	return "", nil, nil
}
