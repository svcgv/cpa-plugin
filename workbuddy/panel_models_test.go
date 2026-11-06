package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCreditsFollowCatalogAndSurviveRefreshFailure(t *testing.T) {
	store := newModelStore(t.TempDir())
	failed := false
	do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			return &hostHTTPResponse{StatusCode: 200, Headers: make(http.Header), Body: []byte(`{"vendor/a":{"id":"a"},"vendor/b":{"id":"b"}}`)}, nil
		}
		if failed {
			return nil, fmt.Errorf("unavailable")
		}
		return &hostHTTPResponse{StatusCode: 200, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["a","b"]}],"models":[{"id":"a","credits":"x0.05","maxInputTokens":200000},{"id":"b","credits":{"invalid":1}},{"id":"hidden","credits":"x8"}]}}`)}, nil
	}
	runtime := newModelRuntime(store, do)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	req := authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "rate-auth", StorageJSON: mustJSON(sa)}}
	got := runtime.ensureForAuth(req)
	if got.State != modelReady || len(got.CatalogFacts) != 2 || got.CatalogFacts[0].Credits != "x0.05" || got.CatalogFacts[1].Credits != "" {
		t.Fatalf("fresh snapshot: %#v", got)
	}
	got.CatalogFacts[0].Credits = "changed"
	if runtime.snapshotForAuthID("rate-auth").CatalogFacts[0].Credits != "x0.05" {
		t.Fatal("snapshot aliases caller data")
	}
	failed = true
	got = runtime.ensureForAuthRefresh(req, true)
	if got.State != modelStale || got.CatalogFacts[0].Credits != "x0.05" || got.CatalogFacts[0].MaxInputContext == nil || *got.CatalogFacts[0].MaxInputContext != 200000 || got.ModelSource != modelSourceCache {
		t.Fatalf("cached snapshot: %#v", got)
	}
}

func TestLegacyCreditsAreOptional(t *testing.T) {
	facts, err := parseWorkBuddyLegacyModels([]byte(`{"code":0,"data":{"models":[{"id":"a","credits":"x0.5"},{"id":"b","credits":5}]}}`))
	if err != nil || len(facts) != 2 || facts[0].Credits != "x0.5" || facts[1].Credits != "" {
		t.Fatalf("facts=%#v error=%v", facts, err)
	}
}

func TestForceRefreshKeepsUsableSnapshotDuringFetch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	block := false
	do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
		if req.URL.Host == "models.dev" {
			return &hostHTTPResponse{StatusCode: 200, Headers: make(http.Header), Body: []byte(`{"vendor/a":{"id":"a"}}`)}, nil
		}
		if block {
			close(entered)
			<-release
		}
		return &hostHTTPResponse{StatusCode: 200, Headers: make(http.Header), Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["a"]}],"models":[{"id":"a","credits":"x0.05"}]}}`)}, nil
	}
	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	req := authModelRequestWire{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "refresh", StorageJSON: mustJSON(syntheticStoredAuth(t, workBuddyRealmCN))}}
	if first := runtime.ensureForAuth(req); first.State != modelReady {
		t.Fatalf("initial %#v", first)
	}
	block = true
	done := make(chan modelReadinessSnapshot, 1)
	go func() { done <- runtime.ensureForAuthRefresh(req, true) }()
	<-entered
	during := runtime.ensureForAuth(req)
	if during.State != modelReady || len(during.Models) != 1 {
		t.Fatalf("during refresh %#v", during)
	}
	close(release)
	if final := <-done; final.State != modelReady {
		t.Fatalf("after refresh %#v", final)
	}
	runtime.storeError = modelErrorCacheRead
	fallback := runtime.ensureForAuthRefresh(req, true)
	if fallback.State != modelStale || len(fallback.Models) != 1 || fallback.CatalogFacts[0].Credits != "x0.05" {
		t.Fatalf("memory fallback %#v", fallback)
	}
}

func TestInvalidOptionalV3MetadataDoesNotBreakCatalog(t *testing.T) {
	for _, metadata := range []string{`{"not":"a list"}`, `[{"id": 1, "credits":"x1"}]`} {
		facts, err := parseWorkBuddyV3Config([]byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["a"]}],"models":` + metadata + `}}`))
		if err != nil || len(facts) != 1 || facts[0].Credits != "" {
			t.Fatalf("facts=%#v err=%v", facts, err)
		}
	}
}

func TestPanelContextValuesFromCatalogAndModelMetadata(t *testing.T) {
	facts, err := parseWorkBuddyV3Config([]byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["a","b","c"]}],"models":[{"id":"a","credits":"x0.05","maxInputTokens":200000},{"id":"b","maxInputTokens":{"bad":1}},{"id":"c","maxInputTokens":-1}]}}`))
	if err != nil || len(facts) != 3 || facts[0].MaxInputContext == nil || *facts[0].MaxInputContext != 200000 || facts[1].MaxInputContext != nil || facts[2].MaxInputContext != nil {
		t.Fatalf("facts=%#v err=%v", facts, err)
	}
	snap := modelReadinessSnapshot{CatalogFacts: facts, Models: []pluginapi.ModelInfo{{ID: "a", ContextLength: 128000}, {ID: "b", ContextLength: 64000}, {ID: "c"}}}
	rows := panelModelsForSnapshot(snap)
	if rows[0]["max_context"] != int64(200000) || rows[1]["max_context"] != int64(64000) {
		t.Fatalf("rows=%#v", rows)
	}
	if _, ok := rows[2]["max_context"]; ok {
		t.Fatalf("unknown context exposed: %#v", rows[2])
	}
	for _, row := range rows {
		if _, ok := row["default_context"]; ok {
			t.Fatalf("invented default: %#v", row)
		}
	}
	cloned := cloneModelReadinessSnapshot(snap)
	*cloned.CatalogFacts[0].MaxInputContext = 1
	if *snap.CatalogFacts[0].MaxInputContext != 200000 {
		t.Fatal("snapshot alias")
	}
}
