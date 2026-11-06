package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Saved catalogs contain no credentials; account identities are hashed.
func catalogCachePath(sa *storedAuth) string {
	if sa == nil || sa.Auth.PersonalToken == "" {
		return ""
	}
	dir := os.Getenv("QODERWORK_CACHE_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(base, "qoderwork")
	}
	key := sha256.Sum256([]byte(modelCacheIdentity(sa)))
	return filepath.Join(dir, fmt.Sprintf("catalog-%x.json", key))
}
func saveCatalog(sa *storedAuth, body []byte) {
	path := catalogCachePath(sa)
	if path == "" || len(body) > 8*1024*1024 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(body); err != nil {
		_ = f.Close()
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	_ = os.Rename(name, path)
}
func loadCatalog(sa *storedAuth) []pluginapi.ModelInfo {
	path := catalogCachePath(sa)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 8*1024*1024 {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	models, err := parseModelListResponse(body)
	if err != nil {
		return nil
	}
	storeCatalogMetadata(sa, body)
	if useMaxContext(sa.region()) {
		for i := range models {
			if m, ok := metadataForModel(sa, models[i].ID); ok {
				models[i].ContextLength = m.maxContextLength()
			}
		}
	}
	return models
}
