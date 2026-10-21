package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQoderEncodeDecodeRoundTrip(t *testing.T) {
	for _, plain := range []string{"{}", `{"chat":[]}`, "hello world", "a"} {
		encoded := qoderEncode([]byte(plain))
		decoded, err := qoderDecode([]byte(encoded))
		if err != nil {
			t.Fatalf("qoderDecode(%q): %v", encoded, err)
		}
		if string(decoded) != plain {
			t.Fatalf("round trip: got %q want %q", decoded, plain)
		}
	}
}

func TestParseModelListResponseFormats(t *testing.T) {
	const catalog = `{"chat":[{"key":"gmodel","display_name":"GLM-6","enable":true,"max_output_tokens":16384,"context_config":{"default":{"token_count":200000,"is_default":true}}},{"key":"disabled","enable":false}]}`
	for name, body := range map[string]string{
		"plain":      catalog,
		"encoded":    qoderEncode([]byte(catalog)),
		"envelope":   `{"code":0,"data":` + catalog + `}`,
		"inlineOnly": `{"inline":[{"key":"imodel","display_name":"Inline","enable":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			models, err := parseModelListResponse([]byte(body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(models) == 0 {
				t.Fatal("no models parsed")
			}
			if models[0].ID == "gmodel" {
				if models[0].Name != "GLM-6" || models[0].ContextLength != 200000 || models[0].MaxCompletionTokens != 16384 {
					t.Fatalf("gmodel fields = %#v", models[0])
				}
			}
		})
	}
}

func TestModelListRequestsStartWithCurrentCLIEndpoint(t *testing.T) {
	reqs := modelListRequests()
	if len(reqs) == 0 {
		t.Fatal("no request variants")
	}
	first := reqs[0]
	if first.Method != http.MethodGet {
		t.Fatalf("first method = %s want GET", first.Method)
	}
	if !strings.Contains(first.URL, "/api/v2/model/list") {
		t.Fatalf("first URL is not the CLI 1.1.69 catalog path: %q", first.URL)
	}
	if !strings.Contains(first.URL, "Encode=1") {
		t.Fatalf("first URL must request Encode=1: %q", first.URL)
	}
	if first.SignBody != "" || len(first.SendBody) != 0 {
		t.Fatalf("first request must sign and send an empty body: %#v", first)
	}
	var sawLegacy bool
	for _, r := range reqs[1:] {
		if strings.Contains(r.URL, "/algo/api/v2/model/list") {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Fatal("legacy /algo catalog path missing from fallbacks")
	}
}

func TestParseModelListResponseAssistantScene(t *testing.T) {
	const catalog = `{"assistant":[{"key":"qmodel_latest","display_name":"Qwen3.8-Max","format":"openai","enable":1,"is_reasoning":true,"efforts":["low","high"],"default_effort":"high","context_config":{"200k":{"token_count":200000,"is_default":true},"1m":{"token_count":1000000}},"max_output_tokens":65536},{"key":"disabled","display_name":"Disabled","enable":0}],"chat":[{"key":"qmodel_latest","display_name":"dup"},{"key":"chat_only","display_name":"Chat Only","enable":true}]}`
	models, err := parseModelListResponse([]byte(catalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v want 2 (duplicate collapsed, disabled skipped)", models)
	}
	if models[0].ID != "qmodel_latest" || models[0].Name != "Qwen3.8-Max" {
		t.Fatalf("first model = %#v", models[0])
	}
	if models[0].ContextLength != 200000 || models[0].MaxCompletionTokens != 65536 {
		t.Fatalf("assistant model fields = %#v", models[0])
	}
	if models[1].ID != "chat_only" {
		t.Fatalf("chat scene not merged: %#v", models)
	}
}

func TestParseModelListResponseEncodedAssistant(t *testing.T) {
	const catalog = `{"assistant":[{"key":"gmodel","display_name":"GLM-6","enable":true,"context_config":{"default":{"token_count":200000,"is_default":true}},"max_output_tokens":16384}]}`
	models, err := parseModelListResponse([]byte(qoderEncode([]byte(catalog))))
	if err != nil {
		t.Fatalf("parse encoded: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gmodel" || models[0].ContextLength != 200000 {
		t.Fatalf("models = %#v", models)
	}
}

func TestCallModelsAPIUsesCurrentCatalogPath(t *testing.T) {
	var gotBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(body))
		if r.Method != http.MethodGet {
			t.Errorf("method = %s want GET", r.Method)
		}
		if got := r.URL.Query().Get("Encode"); got != "1" {
			t.Errorf("Encode = %q want 1", got)
		}
		if got := r.Header.Get("Cosy-Sigpath"); got != "/v2/model/list" {
			t.Errorf("Cosy-Sigpath = %q want /v2/model/list", got)
		}
		if got := r.Header.Get("Cosy-Machinetype"); got != "5" {
			t.Errorf("Cosy-Machinetype = %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Errorf("missing COSY authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assistant":[{"key":"gmodel","display_name":"GLM-6","enable":true}]}`))
	}))
	defer server.Close()

	oldEndpoint := endpointModels
	endpointModels = server.URL + "/api/v2/model/list"
	t.Cleanup(func() { endpointModels = oldEndpoint })
	oldClient := sharedHTTPClient()
	sharedClient = server.Client()
	t.Cleanup(func() { sharedClient = oldClient })

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "jt-test", RefreshToken: "jrt-test", Domain: "qoder.com.cn"},
		Account: storedAccount{UID: "uid-test", Nickname: "tester"},
	}
	models, err := callModelsAPI(sa)
	if err != nil {
		t.Fatalf("callModelsAPI: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gmodel" {
		t.Fatalf("models = %#v", models)
	}
	if len(gotBodies) == 0 || gotBodies[0] != "" {
		t.Fatalf("first request body = %q want empty", strings.Join(gotBodies, ","))
	}
}

func TestCallModelsAPIFallsBackToBareCatalogPath(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		// Pretend this build only routes the bare /api form.
		if strings.Contains(r.URL.Path, "/algo/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"404"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assistant":[{"key":"bare","display_name":"Bare","enable":true}]}`))
	}))
	defer server.Close()

	oldEndpoint := endpointModels
	endpointModels = server.URL + "/algo/api/v2/model/list"
	t.Cleanup(func() { endpointModels = oldEndpoint })
	oldClient := sharedHTTPClient()
	sharedClient = server.Client()
	t.Cleanup(func() { sharedClient = oldClient })

	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-test"}, Account: storedAccount{UID: "uid-test"}}
	models, err := callModelsAPI(sa)
	if err != nil {
		t.Fatalf("callModelsAPI: %v", err)
	}
	if len(models) != 1 || models[0].ID != "bare" {
		t.Fatalf("models = %#v", models)
	}
	if len(paths) < 2 || !strings.HasSuffix(paths[1], "/api/v2/model/list") || strings.Contains(paths[1], "/algo/") {
		t.Fatalf("expected bare /api fallback, paths=%v", paths)
	}
}

func TestParseModelListResponseTolerantEntryTypes(t *testing.T) {
	// Mirrors gateway shapes seen in the wild: string token counts, numeric
	// enable flags, and one malformed record that must not discard its scene.
	const catalog = `{"assistant":[{"key":"qmodel_latest","display_name":"Qwen3.8-Max","format":"openai","enable":true,"context_config":{"200k":{"token_count":"200000","is_default":true},"1m":{"token_count":1000000}},"max_output_tokens":"65536"},{"key":"dmodel","display_name":"DeepSeek-V4-Pro","format":"anthropic","enable":1,"max_input_tokens":"128000","max_output_tokens":8192},{"key":"off","display_name":"Off","format":"openai","enable":0},{"key":123,"display_name":"Bad"}],"chat":[{"key":"kmodel","display_name":"Kimi-K2.7","format":"openai","enable":true}]}`
	models, err := parseModelListResponse([]byte(catalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %#v want 3", models)
	}
	if models[0].ID != "qmodel_latest" || models[0].ContextLength != 200000 || models[0].MaxCompletionTokens != 65536 {
		t.Fatalf("first model = %#v", models[0])
	}
	if models[1].ID != "dmodel" || models[1].ContextLength != 128000 {
		t.Fatalf("second model = %#v", models[1])
	}
	enc, err := parseModelListResponse([]byte(qoderEncode([]byte(catalog))))
	if err != nil || len(enc) != 3 {
		t.Fatalf("encoded: %v, models=%d", err, len(enc))
	}
}

func TestDynamicModelCacheIsPerAccount(t *testing.T) {
	clearDynamicModelsCache()
	t.Cleanup(clearDynamicModelsCache)

	storeDynamicModelsFor("cn:uid-a", []pluginapi.ModelInfo{{ID: "a-model"}})
	storeDynamicModelsFor("cn:uid-b", []pluginapi.ModelInfo{{ID: "b-model"}})

	a, ok := cachedDynamicModelsFor("cn:uid-a")
	if !ok || len(a) != 1 || a[0].ID != "a-model" {
		t.Fatalf("uid-a cache = %#v", a)
	}
	b, ok := cachedDynamicModelsFor("cn:uid-b")
	if !ok || len(b) != 1 || b[0].ID != "b-model" {
		t.Fatalf("uid-b cache = %#v", b)
	}
	if _, ok := cachedDynamicModelsFor("cn:uid-c"); ok {
		t.Fatal("unexpected cache hit for another account")
	}
}

func TestCosySessionUsesPersistedMachineID(t *testing.T) {
	cosySessionCache = sync.Map{}
	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-machine", MachineID: "machine-abc"}, Account: storedAccount{UID: "uid-machine"}}
	sess, err := cosySessionFor(sa)
	if err != nil {
		t.Fatal(err)
	}
	if sess.MachineID != "machine-abc" {
		t.Fatalf("MachineID = %q want machine-abc", sess.MachineID)
	}

	legacy := &storedAuth{Auth: storedTokens{AccessToken: "jt-legacy"}, Account: storedAccount{UID: "uid-legacy"}}
	first, err := cosySessionFor(legacy)
	if err != nil {
		t.Fatal(err)
	}
	cosySessionCache = sync.Map{}
	second, err := cosySessionFor(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if first.MachineID == "" || first.MachineID != second.MachineID {
		t.Fatalf("legacy MachineID not stable: %q vs %q", first.MachineID, second.MachineID)
	}
}

func TestReadOfficialMachineIdentity(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "SharedClientCache", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "id"), []byte("machine-from-cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "machine_token.json"), []byte(`{"token":"tok-1","type":"5","os":"aarch64_darwin","version":"1.1.28"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := qoderUserDataDirsOverride
	qoderUserDataDirsOverride = []string{root}
	t.Cleanup(func() { qoderUserDataDirsOverride = old })

	got := readOfficialMachineIdentity()
	if got.MachineID != "machine-from-cache" {
		t.Fatalf("MachineID = %q", got.MachineID)
	}
	if got.MachineToken != "tok-1" || got.MachineType != "5" || got.MachineOS != "aarch64_darwin" || got.CosyVersion != "1.1.28" {
		t.Fatalf("identity = %#v", got)
	}

	// Headers must adopt the cached identity when present.
	cosySessionCache = sync.Map{}
	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-cache"}, Account: storedAccount{UID: "uid-cache"}}
	sess, err := cosySessionFor(sa)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := sess.modelListHeaders("uid-cache", "https://gateway.qoder.com.cn/algo/api/v2/model/list")
	if err != nil {
		t.Fatal(err)
	}
	if hdr["cosy-machinetoken"] != "tok-1" || hdr["cosy-version"] != "1.1.28" {
		t.Fatalf("headers = %#v", hdr)
	}
}
