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

func TestModelListRequestsStartWithBodylessGET(t *testing.T) {
	reqs := modelListRequests()
	if len(reqs) == 0 {
		t.Fatal("no request variants")
	}
	first := reqs[0]
	if first.Method != http.MethodGet {
		t.Fatalf("first method = %s want GET", first.Method)
	}
	if strings.Contains(first.URL, "Encode=1") {
		t.Fatalf("first URL still carries legacy Encode=1: %q", first.URL)
	}
	if first.SignBody != "" || len(first.SendBody) != 0 {
		t.Fatalf("first request must sign and send an empty body: %#v", first)
	}
}

func TestCallModelsAPISignsBodylessGETAndParsesCatalog(t *testing.T) {
	var gotBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, string(body))
		if r.Method != http.MethodGet {
			t.Errorf("method = %s want GET", r.Method)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q want empty", r.URL.RawQuery)
		}
		if got := r.Header.Get("Cosy-Sigpath"); got != "/api/v2/model/list" {
			t.Errorf("Cosy-Sigpath = %q", got)
		}
		if got := r.Header.Get("Cosy-Machinetype"); got != "5" {
			t.Errorf("Cosy-Machinetype = %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Errorf("missing COSY authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"gmodel","display_name":"GLM-6","enable":true}]}`))
	}))
	defer server.Close()

	oldEndpoint := endpointModels
	endpointModels = server.URL + "/algo/api/v2/model/list"
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

func TestCallModelsAPIFallsBackToLegacyQuery(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("Encode") != "1" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"101","message":"Signature invalid"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"chat":[{"key":"legacy","display_name":"Legacy","enable":true}]}}`))
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
	if len(models) != 1 || models[0].ID != "legacy" {
		t.Fatalf("models = %#v", models)
	}
	if calls < 2 {
		t.Fatalf("expected a fallback attempt, calls=%d", calls)
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
