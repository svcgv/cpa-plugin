package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostCallForTest is an injectable boundary around the C ABI host callback.
// Production leaves it nil; regression tests use it to exercise the complete
// host.auth.list/get/save flow without loading a c-shared plugin.
var (
	hostCallHookMu      sync.RWMutex
	hostCallHookForTest func(string, []byte) ([]byte, error)
)

func currentHostCallHook() func(string, []byte) ([]byte, error) {
	hostCallHookMu.RLock()
	defer hostCallHookMu.RUnlock()
	return hostCallHookForTest
}

func setHostCallHook(fn func(string, []byte) ([]byte, error)) func(string, []byte) ([]byte, error) {
	hostCallHookMu.Lock()
	defer hostCallHookMu.Unlock()
	old := hostCallHookForTest
	hostCallHookForTest = fn
	return old
}

// priorityAttributeValue normalizes a numeric priority to the integer string
// used by CPA routing attributes. Invalid values are left untouched in the
// file and omitted from host metadata/attributes.
func priorityAttributeValue(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		s := strings.TrimSpace(v)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return strconv.FormatInt(n, 10), true
		}
	case json.Number:
		return priorityAttributeValue(v.String())
	case int:
		return strconv.FormatInt(int64(v), 10), true
	case int8:
		return strconv.FormatInt(int64(v), 10), true
	case int16:
		return strconv.FormatInt(int64(v), 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint8:
		return strconv.FormatUint(uint64(v), 10), true
	case uint16:
		return strconv.FormatUint(uint64(v), 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case float32:
		return priorityAttributeValue(float64(v))
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v == math.Trunc(v) && v >= -1<<53 && v <= 1<<53 {
			return strconv.FormatInt(int64(v), 10), true
		}
	}
	return "", false
}

func priorityValueFromRaw(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

// decodeAuthDocument requires an existing, readable JSON object. A failed read
// or parse must abort the operation rather than replace the file with a
// freshly initialized credential document.
func decodeAuthDocument(physical []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(physical, &doc); err != nil {
		return nil, fmt.Errorf("parse existing auth file: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("parse existing auth file: empty JSON object")
	}
	return doc, nil
}

func encodeAuthDocument(doc map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode auth file: %w", err)
	}
	return raw, nil
}

func putAuthDocumentField(doc map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode auth field %q: %w", key, err)
	}
	doc[key] = raw
	return nil
}

// mergeAuthFileJSON starts with the complete physical document. Only sa's
// nested credential fields and the caller's explicit updates are replaced, so
// priority and every other host-managed field survive the round trip.
func mergeAuthFileJSON(physical []byte, sa *storedAuth, updates map[string]any) ([]byte, error) {
	doc, err := decodeAuthDocument(physical)
	if err != nil {
		return nil, err
	}
	if sa != nil {
		storage, err := json.Marshal(sa)
		if err != nil {
			return nil, err
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(storage, &nested); err != nil {
			return nil, err
		}
		for _, key := range []string{"type", "provider", "logo"} {
			if _, exists := doc[key]; !exists {
				if err := putAuthDocumentField(doc, key, defaultAuthField(key)); err != nil {
					return nil, err
				}
			}
		}
		doc["auth"] = nested["auth"]
		doc["account"] = nested["account"]
	}
	for key, value := range updates {
		if err := putAuthDocumentField(doc, key, value); err != nil {
			return nil, err
		}
	}
	return encodeAuthDocument(doc)
}

func defaultAuthField(key string) any {
	switch key {
	case "type", "provider":
		return providerName
	case "logo":
		return pluginLogoURL
	default:
		return nil
	}
}

func isHostCredentialField(key string) bool {
	switch key {
	case "auth", "account", "accessToken", "refreshToken", "expiresAt", "domain",
		"personalToken", "region", "machineId", "uid", "enterpriseId", "nickname":
		return true
	default:
		return false
	}
}

func cloneHostAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneHostStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// patchAuthFileDocument changes lifecycle-owned fields on an already-read
// physical document. Credentials and all other metadata are left untouched.
func patchAuthFileDocument(sa *storedAuth, phys *hostAuthPhysical, updates map[string]any) error {
	if phys == nil {
		return fmt.Errorf("existing auth file could not be read")
	}
	raw, err := mergeAuthFileJSON(phys.JSON, nil, updates)
	if err != nil {
		return err
	}
	// Lifecycle still follows the existing legacy-to-UID migration rule. The
	// merge itself never rewrites credentials from its in-memory storedAuth.
	name, path, legacyPath := resolveAuthFileTarget(sa, phys)
	return hostAuthPersistMigrate(name, path, legacyPath, raw)
}

// existingAuthFileName keeps a matched account's physical identity. New
// accounts receive the canonical UID-based file name.
func existingAuthFileName(phys *hostAuthPhysical, canonical string) string {
	if phys != nil && strings.TrimSpace(phys.Name) != "" {
		return strings.TrimSpace(phys.Name)
	}
	return canonical
}

// authDocumentBundle carries both the JSON to save and the physical identity of
// an existing account, so callers can preserve custom filenames and migrations.
type authDocumentBundle struct {
	JSON     []byte
	Physical *hostAuthPhysical
}

// prepareImportedPATDocument finds an existing credential for the same UID
// and region, preserving its full physical document on repeated import.
func prepareImportedPATDocument(sa *storedAuth, region qoderRegion) (*authDocumentBundle, error) {
	return findExistingOrNewAuthDocument(sa, func(previous *storedAuth) bool {
		return previous != nil && previous.Account.UID == sa.Account.UID && previous.region() == region
	})
}

// findExistingOrNewAuthDocument returns a merged document for the first
// matching existing account, or a new-account initializer when none matches.
// Existing updates never overwrite note, disabled, priority or unknown fields.
func findExistingOrNewAuthDocument(sa *storedAuth, match func(*storedAuth) bool) (*authDocumentBundle, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	files, err := hostAuthList()
	if err != nil {
		return nil, fmt.Errorf("read existing auth files: %w", err)
	}
	for _, f := range files {
		phys, err := hostAuthGetPhysical(f.AuthIndex)
		if err != nil {
			return nil, fmt.Errorf("read existing auth file: %w", err)
		}
		if _, err := decodeAuthDocument(phys.JSON); err != nil {
			return nil, err
		}
		previous, err := parseStored(phys.JSON)
		if err != nil {
			return nil, fmt.Errorf("parse existing auth file: %w", err)
		}
		if !match(previous) {
			continue
		}
		raw, err := mergeAuthFileJSON(phys.JSON, sa, nil)
		if err != nil {
			return nil, err
		}
		return &authDocumentBundle{JSON: raw, Physical: phys}, nil
	}
	raw, err := buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
	if err != nil {
		return nil, err
	}
	return &authDocumentBundle{JSON: raw}, nil
}

// currentHostMetadata uses the values supplied by the host as authoritative.
// Credential containers are excluded because pluginTokenStorage overlays this
// map on StorageJSON when it writes the refreshed credential file.
func currentHostMetadata(host map[string]any) map[string]any {
	metadata := cloneHostAnyMap(host)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	for key := range host {
		if isHostCredentialField(key) {
			delete(metadata, key)
		}
	}
	return metadata
}

// authDataWithHostState overlays the host's current metadata and attributes on
// a refreshed AuthData. Current host values win over stale storage values;
// credential containers are excluded from Metadata so they cannot overwrite
// the renewed credentials when the host persists its token storage.
func authDataWithHostState(ad pluginapi.AuthData, req pluginapi.AuthRefreshRequest) pluginapi.AuthData {
	storage, storageErr := mergeRefreshedStorageJSON(req.StorageJSON, ad.StorageJSON)
	if storageErr != nil {
		return ad
	}
	ad.StorageJSON = storage
	doc, err := decodeAuthDocument(req.StorageJSON)
	if err != nil {
		return ad
	}
	metadata := currentHostMetadata(req.Metadata)
	for key, value := range ad.Metadata {
		if key == "type" || key == "provider" {
			metadata[key] = value
		}
	}
	ad.Metadata = metadata
	if _, exists := metadata["note"]; !exists {
		if value, ok := priorityValueFromRaw(doc["note"]); ok {
			metadata["note"] = value
		}
	}
	if _, exists := metadata["disabled"]; !exists {
		if value, ok := priorityValueFromRaw(doc["disabled"]); ok {
			metadata["disabled"] = value
		}
	}
	ad.Attributes = cloneHostStringMap(req.Attributes)
	if ad.Attributes == nil {
		ad.Attributes = make(map[string]string)
	}
	if value, ok := ad.Metadata["priority"]; ok {
		if _, valid := priorityAttributeValue(value); !valid {
			delete(ad.Metadata, "priority")
		} else if _, exists := ad.Attributes["priority"]; !exists {
			if normalized, valid := priorityAttributeValue(value); valid {
				ad.Attributes["priority"] = normalized
			}
		}
	}
	if attributePriority, ok := ad.Attributes["priority"]; ok {
		if _, valid := priorityAttributeValue(attributePriority); !valid {
			delete(ad.Attributes, "priority")
		}
	}
	if disabled, ok := ad.Metadata["disabled"].(bool); ok {
		ad.Disabled = disabled
	}
	return ad
}

// mergeRefreshedStorageJSON keeps the host's full current document as the
// baseline and replaces only the nested credentials. pluginTokenStorage then
// overlays noncredential Metadata on this document when it writes the file.
func mergeRefreshedStorageJSON(current, refreshed []byte) ([]byte, error) {
	doc, err := decodeAuthDocument(current)
	if err != nil {
		return nil, err
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(refreshed, &nested); err != nil {
		return nil, fmt.Errorf("parse refreshed credentials: %w", err)
	}
	for _, key := range []string{"type", "provider", "logo"} {
		if _, exists := doc[key]; !exists {
			if err := putAuthDocumentField(doc, key, defaultAuthField(key)); err != nil {
				return nil, err
			}
		}
	}
	doc["auth"] = nested["auth"]
	doc["account"] = nested["account"]
	return encodeAuthDocument(doc)
}

// authDataWithParsedFileState preserves the complete physical document in
// StorageJSON and carries priority into both metadata and routing attributes.
func authDataWithParsedFileState(ad pluginapi.AuthData, raw []byte) pluginapi.AuthData {
	doc, err := decodeAuthDocument(raw)
	if err != nil {
		return ad
	}
	ad.StorageJSON = append([]byte(nil), raw...)
	ad.Label = parsedAuthString(ad.Label, doc["label"])
	ad.Prefix = parsedAuthString(ad.Prefix, doc["prefix"])
	ad.ProxyURL = parsedAuthString(ad.ProxyURL, doc["proxy_url"])
	ad.Metadata = parsedAuthMetadata(ad.Metadata, doc)
	if value, ok := priorityValueFromRaw(doc["priority"]); ok {
		priority, valid := priorityAttributeValue(value)
		if valid {
			ad.Metadata["priority"] = value
			if ad.Attributes == nil {
				ad.Attributes = make(map[string]string)
			}
			ad.Attributes["priority"] = priority
		}
	}
	return ad
}

func parsedAuthString(current string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return current
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil && strings.TrimSpace(value) != "" {
		return value
	}
	return current
}

// parsedAuthMetadata mirrors noncredential physical fields into host metadata.
// pluginTokenStorage overlays Metadata on StorageJSON when writing; omitting
// auth/account here ensures a later refresh writes the renewed credentials.
func parsedAuthMetadata(current map[string]any, doc map[string]json.RawMessage) map[string]any {
	metadata := cloneHostAnyMap(current)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	for key, raw := range doc {
		if isHostCredentialField(key) || key == "note" || key == "disabled" {
			continue
		}
		if value, ok := priorityValueFromRaw(raw); ok {
			metadata[key] = value
		}
	}
	if value, ok := priorityValueFromRaw(doc["priority"]); !ok {
		delete(metadata, "priority")
	} else if _, valid := priorityAttributeValue(value); !valid {
		delete(metadata, "priority")
	}
	if rawNote, ok := doc["note"]; ok {
		var note string
		if json.Unmarshal(rawNote, &note) == nil {
			metadata["note"] = note
		}
	}
	if rawDisabled, ok := doc["disabled"]; ok {
		var disabled bool
		if json.Unmarshal(rawDisabled, &disabled) == nil {
			metadata["disabled"] = disabled
		}
	}
	return metadata
}

// authDataWithExistingFileState adapts newly authorized credentials to the
// document that the host will write after an OAuth login poll. It preserves
// the existing file name, priority and other host-managed state without a
// second plugin-owned save.
func authDataWithExistingFileState(ad pluginapi.AuthData, phys *hostAuthPhysical, newStorage []byte) pluginapi.AuthData {
	if phys == nil {
		return ad
	}
	previous, err := parseStored(phys.JSON)
	if err != nil {
		return ad
	}
	ad.FileName = existingAuthFileName(phys, authFileNameFor(previous))
	doc, err := decodeAuthDocument(phys.JSON)
	if err != nil {
		return ad
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(newStorage, &nested); err != nil {
		return ad
	}
	doc["auth"] = nested["auth"]
	doc["account"] = nested["account"]
	if raw, err := encodeAuthDocument(doc); err == nil {
		ad.StorageJSON = raw
	}
	ad.Metadata = parsedAuthMetadata(ad.Metadata, doc)
	if value, ok := priorityValueFromRaw(doc["priority"]); ok {
		if priority, valid := priorityAttributeValue(value); valid {
			ad.Metadata["priority"] = value
			if ad.Attributes == nil {
				ad.Attributes = make(map[string]string)
			}
			ad.Attributes["priority"] = priority
		}
	}
	if disabled, ok := ad.Metadata["disabled"].(bool); ok {
		ad.Disabled = disabled
	}
	return ad
}
