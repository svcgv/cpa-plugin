package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegionClassification(t *testing.T) {
	cases := []struct {
		domain string
		want   qoderRegion
	}{
		{"", regionCN},
		{"qoder.com.cn", regionCN},
		{"qoder.sh", regionIntl},
		{"openapi.qoder.sh", regionIntl},
		{"qoder.com", regionIntl},
	}
	for _, c := range cases {
		sa := &storedAuth{Auth: storedTokens{Domain: c.domain}}
		if got := sa.region(); got != c.want {
			t.Errorf("domain %q -> %q, want %q", c.domain, got, c.want)
		}
	}
	// Region field wins over domain.
	sa := &storedAuth{Auth: storedTokens{Domain: "qoder.com.cn", Region: "intl"}}
	if got := sa.region(); got != regionIntl {
		t.Errorf("explicit region field ignored: got %q", got)
	}
}

func TestEndpointsForRegion(t *testing.T) {
	cn := endpointsForRegion(regionCN)
	if cn.OpenAPI != upstreamBaseCN || cn.Gateway != gatewayBaseCN || cn.Website != qoderWebsiteCN {
		t.Fatalf("cn endpoints = %#v", cn)
	}
	if cn.RedirectURI != qoderRedirectURI {
		t.Fatalf("cn redirect = %q", cn.RedirectURI)
	}
	in := endpointsForRegion(regionIntl)
	if in.OpenAPI != qoderIntlOpenAPI || in.Gateway != qoderIntlGateway || in.Website != qoderIntlWebsite {
		t.Fatalf("intl endpoints = %#v", in)
	}
	if in.RedirectURI != qoderIntlRedirectURI {
		t.Fatalf("intl redirect = %q", in.RedirectURI)
	}
}

func TestParseRegionValue(t *testing.T) {
	for _, v := range []string{"intl", "INTL", "global", "qoder.sh", "international"} {
		if got := parseRegionValue(v); got != regionIntl {
			t.Errorf("parseRegionValue(%q) = %q want intl", v, got)
		}
	}
	for _, v := range []string{"", "cn", "国内版", "unknown"} {
		if got := parseRegionValue(v); got != regionCN {
			t.Errorf("parseRegionValue(%q) = %q want cn", v, got)
		}
	}
}

func TestIntlUpstreamModel(t *testing.T) {
	cases := map[string]string{
		"":                  "auto",
		"qoder-auto":        "auto",
		"claude-sonnet-4.5": "claude-sonnet-4.5",
		"sonnet":            "claude-sonnet-4.5",
		"claude-opus-4.1":   "claude-opus-4.1",
		"gpt-5":             "gpt-5",
		"gemini-2.5-pro":    "gemini-2.5-pro",
		"unknown-model":     "unknown-model",
	}
	for in, want := range cases {
		if got := intlUpstreamModel(in); got != want {
			t.Errorf("intlUpstreamModel(%q) = %q want %q", in, got, want)
		}
	}
}

func TestBuildIntlChatBodyForcesStream(t *testing.T) {
	in := []byte(`{"model":"old","messages":[{"role":"user","content":"hi"}],"stream":false,"temperature":0.2}`)
	out, err := buildIntlChatBody(in, "claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-sonnet-4.5" {
		t.Errorf("model = %v", got["model"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v want true", got["stream"])
	}
	if got["temperature"] != 0.2 {
		t.Errorf("temperature not preserved: %v", got["temperature"])
	}
	if _, ok := got["messages"].([]any); !ok {
		t.Errorf("messages not preserved: %v", got["messages"])
	}
}

func TestParseIntlModelListOpenAIShape(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"claude-sonnet-4.5","owned_by":"qoder"},{"id":"gpt-5"}]}`)
	models, err := parseIntlModelList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "claude-sonnet-4.5" {
		t.Fatalf("models = %#v", models)
	}
	if models[1].Name != "gpt-5" {
		t.Fatalf("name fallback failed: %#v", models[1])
	}
}

func TestParseIntlModelListSceneShape(t *testing.T) {
	body := []byte(`{"chat":[{"key":"qmodel","display_name":"Qwen","enable":true}]}`)
	models, err := parseIntlModelList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "qmodel" {
		t.Fatalf("models = %#v", models)
	}
}

func TestIntlHeadersUseBearerAndMachineIdentity(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-intl", MachineID: "machine-xyz", Region: "intl"}}
	h := intlHeaders(sa)
	if h["Authorization"] != "Bearer jt-intl" {
		t.Errorf("auth = %q", h["Authorization"])
	}
	if h["Cosy-Machineid"] != "machine-xyz" {
		t.Errorf("machineid = %q", h["Cosy-Machineid"])
	}
	if !strings.HasPrefix(h["Cosy-Version"], "1.") {
		t.Errorf("cosy-version = %q", h["Cosy-Version"])
	}
}

func TestCallIntlModelsAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != intlModelListPath {
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer jt-intl" {
			t.Errorf("auth = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4.5"}]}`))
	}))
	defer server.Close()

	old := qoderIntlGatewayOverride
	qoderIntlGatewayOverride = server.URL
	t.Cleanup(func() { qoderIntlGatewayOverride = old })
	oldClient := sharedHTTPClient()
	sharedClient = server.Client()
	t.Cleanup(func() { sharedClient = oldClient })

	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-intl", Region: "intl"}}
	models, err := callIntlModelsAPI(sa)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "claude-sonnet-4.5" {
		t.Fatalf("models = %#v", models)
	}
}

func TestCallIntlModelsAPIPrefersCOSYCatalog(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if !strings.HasPrefix(r.URL.Path, "/algo/api/v2/model/list") {
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer COSY.") {
			t.Errorf("catalog auth = %q want COSY", got)
		}
		_, _ = w.Write([]byte(`{"assistant":[{"key":"claude-sonnet-4.6","display_name":"Claude Sonnet 4.6","format":"anthropic","enable":true}]}`))
	}))
	defer server.Close()

	oldCatalog := intlCatalogGatewayOverride
	intlCatalogGatewayOverride = server.URL
	t.Cleanup(func() { intlCatalogGatewayOverride = oldCatalog })
	oldClient := sharedHTTPClient()
	sharedClient = server.Client()
	t.Cleanup(func() { sharedClient = oldClient })

	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-intl", Region: "intl"}, Account: storedAccount{UID: "uid-intl"}}
	models, err := callIntlModelsAPI(sa)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "claude-sonnet-4.6" {
		t.Fatalf("models = %#v", models)
	}
	if len(paths) == 0 || !strings.HasPrefix(paths[0], "/algo/api/v2/model/list") {
		t.Fatalf("paths = %v want COSY catalog first", paths)
	}
}

func TestIntlStaticModelsPresent(t *testing.T) {
	ms := intlStaticModels()
	if len(ms) == 0 {
		t.Fatal("no static intl models")
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if m.ID == "" || m.OwnedBy != providerName {
			t.Fatalf("bad model %#v", m)
		}
		seen[m.ID] = true
	}
	if !seen["auto"] {
		t.Fatal("auto missing from intl static models")
	}
	_ = pluginapi.ModelInfo{}
}
