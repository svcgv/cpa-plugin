package main

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
)

var panelModelErrors sync.Map     // account identity -> last refresh error
var panelModelAuthErrors sync.Map // auth index -> sanitized read error

func positivePanelFactor(raw json.RawMessage) (float64, bool) {
	var number float64
	if err := json.Unmarshal(raw, &number); err != nil || number <= 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

// Optional malformed pricing is ignored independently of the model catalog.
func addPanelPricing(row map[string]any, meta modelCatalogEntry) {
	factor, haveFactor := positivePanelFactor(meta.PriceFactor)
	if haveFactor {
		row["price_factor"] = factor
	}
	if original, ok := positivePanelFactor(meta.OriginalPriceFactor); ok && haveFactor && original > factor {
		row["original_price_factor"] = original
	}
	var promotion struct {
		Active      *bool             `json:"active"`
		Description map[string]string `json:"description"`
		Badge       map[string]string `json:"badge"`
		Timezone    string            `json:"timezone"`
		WindowStart string            `json:"window_start"`
		WindowEnd   string            `json:"window_end"`
	}
	if json.Unmarshal(meta.Promotion, &promotion) != nil {
		return
	}
	desc := strings.TrimSpace(promotion.Description["zh"])
	if desc == "" {
		desc = strings.TrimSpace(promotion.Description["en"])
	}
	if desc == "" {
		desc = strings.TrimSpace(promotion.Badge["zh"])
	}
	if desc == "" {
		desc = strings.TrimSpace(promotion.Badge["en"])
	}
	if promotion.Active == nil && desc == "" && promotion.WindowStart == "" && promotion.WindowEnd == "" {
		return
	}
	row["promotion"] = map[string]any{"active": promotion.Active, "description": desc, "timezone": promotion.Timezone, "window_start": promotion.WindowStart, "window_end": promotion.WindowEnd}
}
