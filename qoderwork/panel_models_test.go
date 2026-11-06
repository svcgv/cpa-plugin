package main

import (
	"encoding/json"
	"testing"
)

func TestPanelPricingCacheAndRealmIsolation(t *testing.T) {
	t.Setenv("QODERWORK_CACHE_DIR", t.TempDir())
	cn := &storedAuth{Auth: storedTokens{PersonalToken: "pt-a", Region: "cn"}, Account: storedAccount{UID: "same"}}
	intl := &storedAuth{Auth: storedTokens{PersonalToken: "pt-b", Region: "intl"}, Account: storedAccount{UID: "same"}}
	raw := []byte(`{"assistant":[{"key":"same","display_name":"Same","enable":true,"price_factor":0.05,"original_price_factor":0.5,"promotion":{"active":false,"description":{"zh":"错峰"},"window_start":"22:00","window_end":"08:00","timezone":"Asia/Shanghai"}}]}`)
	saveCatalog(cn, raw)
	if len(loadCatalog(cn)) != 1 {
		t.Fatal("cache failed")
	}
	if _, ok := metadataForModel(intl, "same"); ok {
		t.Fatal("cross-region rate leak")
	}
	m, ok := metadataForModel(cn, "same")
	if !ok {
		t.Fatal("metadata missing")
	}
	row := map[string]any{}
	addPanelPricing(row, m)
	if row["price_factor"] != 0.05 || row["original_price_factor"] != 0.5 {
		t.Fatalf("rates %#v", row)
	}
	promo := row["promotion"].(map[string]any)
	if *(promo["active"].(*bool)) || promo["timezone"] != "Asia/Shanghai" {
		t.Fatalf("promotion %#v", promo)
	}
}
func TestPanelPricingBadOptionalFieldsDoNotInvalidateModel(t *testing.T) {
	raw := []byte(`{"assistant":[{"key":"a","enable":true,"price_factor":{"bad":1},"original_price_factor":-1,"promotion":"bad"}]}`)
	models, err := parseModelListResponse(raw)
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%#v err=%v", models, err)
	}
	var entry modelCatalogEntry
	if err := json.Unmarshal([]byte(`{"price_factor":{"bad":1},"original_price_factor":-1,"promotion":"bad"}`), &entry); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{}
	addPanelPricing(row, entry)
	if len(row) != 0 {
		t.Fatalf("invalid fields: %#v", row)
	}
}
