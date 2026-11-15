package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDeviceCredentialDoesNotRequirePAT(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{
		AccessToken:  "dt-device",
		RefreshToken: "drt-device",
		Region:       "cn",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}}
	if err := preparePATCredential(sa); err != nil {
		t.Fatalf("device credential should be usable without PAT: %v", err)
	}
	if err := forcePATCredential(sa); err != nil {
		t.Fatalf("device credential should not require PAT exchange: %v", err)
	}
	if sa.Auth.AccessToken != "dt-device" || sa.Auth.RefreshToken != "drt-device" {
		t.Fatal("device token family was replaced")
	}
}

func TestModelForAuthPublishesStaticCatalogForDeviceCredential(t *testing.T) {
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  "dt-device",
			RefreshToken: "drt-device",
			Region:       "cn",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		},
		Account: storedAccount{UID: "device-uid"},
	}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(pluginapi.AuthModelRequest{
		AuthID:       "qoderwork-device.json",
		AuthProvider: providerName,
		StorageJSON:  raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := handleModelForAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool                    `json:"ok"`
		Result pluginapi.ModelResponse `json:"result"`
	}
	if err := json.Unmarshal(reply, &env); err != nil || !env.OK {
		t.Fatalf("bad model.for_auth envelope: %v %s", err, redactSecrets(string(reply)))
	}
	if len(env.Result.Models) != len(wbModels()) {
		t.Fatalf("models = %d, want static fallback %d", len(env.Result.Models), len(wbModels()))
	}
	if env.Result.Models[0].ID != "qoder/auto" {
		t.Fatalf("first model = %q, want qoder/auto", env.Result.Models[0].ID)
	}
	if len(env.Result.AuthUpdate.StorageJSON) != 0 {
		t.Fatal("model.for_auth must not emit AuthUpdate and race auth registration")
	}
}

func TestHandleRefreshAuthUsesDeviceTokenEndpoint(t *testing.T) {
	var paths []string
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"token":"dt-new","refresh_token":"drt-new","expires_in":3600000}`))
	})
	sa := &storedAuth{Auth: storedTokens{
		AccessToken:  "dt-old",
		RefreshToken: "drt-old",
		Region:       "cn",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}, Account: storedAccount{UID: "device-uid"}}
	raw, _ := json.Marshal(sa)
	request, _ := json.Marshal(pluginapi.AuthRefreshRequest{
		AuthID:      "qoderwork-device.json",
		StorageJSON: raw,
	})
	reply, err := handleRefreshAuth(request)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool                          `json:"ok"`
		Result pluginapi.AuthRefreshResponse `json:"result"`
	}
	if err := json.Unmarshal(reply, &env); err != nil || !env.OK {
		t.Fatalf("bad auth.refresh envelope: %v %s", err, redactSecrets(string(reply)))
	}
	if len(paths) != 1 || paths[0] != "/api/v1/deviceToken/refresh" {
		t.Fatalf("refresh paths = %#v, want deviceToken/refresh", paths)
	}
	var refreshed storedAuth
	if err := json.Unmarshal(env.Result.Auth.StorageJSON, &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Auth.AccessToken != "dt-new" || refreshed.Auth.RefreshToken != "drt-new" {
		t.Fatalf("refreshed token family = %q/%q", refreshed.Auth.AccessToken, refreshed.Auth.RefreshToken)
	}
}
