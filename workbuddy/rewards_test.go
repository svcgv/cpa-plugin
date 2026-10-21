package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func TestRewardMakeupDateOnlyUsesExplicitZeroCells(t *testing.T) {
	streak := map[string]any{
		"launch_date":  "2026-06-17",
		"streak":       map[string]any{"days": float64(1), "makeup_dates": []any{"2026-09-01"}},
		"makeup_cards": map[string]any{"balance": float64(2)},
	}
	heatmap := map[string]any{
		"today": map[string]any{"date": "2026-09-30"},
		"cells": []any{
			map[string]any{"date": "2026-09-01", "score": float64(10)},
			map[string]any{"date": "2026-09-27", "score": float64(0)},
			map[string]any{"date": "2026-09-28", "score": float64(0)},
			map[string]any{"date": "2026-09-29", "score": float64(4)},
			map[string]any{"date": "2026-09-30", "score": float64(0)},
		},
	}
	got, err := rewardMakeupDate(streak, heatmap)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-09-28" {
		t.Fatalf("date = %q, want latest eligible gap", got)
	}
}

func TestRewardMakeupDateRejectsAmbiguousCalendar(t *testing.T) {
	base := func() (map[string]any, map[string]any) {
		return map[string]any{
			"launch_date": "2026-06-17",
			"streak":      map[string]any{"makeup_dates": []any{}},
		}, map[string]any{
			"today": map[string]any{"date": "2026-09-30"},
			"cells": []any{map[string]any{"date": "2026-09-28", "score": float64(0)}},
		}
	}
	tests := []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{"missing today", func(_ map[string]any, h map[string]any) { delete(h, "today") }},
		{"missing launch", func(s map[string]any, _ map[string]any) { delete(s, "launch_date") }},
		{"missing cells", func(_ map[string]any, h map[string]any) { delete(h, "cells") }},
		{"invalid score", func(_ map[string]any, h map[string]any) {
			h["cells"] = []any{map[string]any{"date": "2026-09-28", "score": "0"}}
		}},
		{"conflicting score", func(_ map[string]any, h map[string]any) {
			h["cells"] = []any{
				map[string]any{"date": "2026-09-28", "score": float64(0)},
				map[string]any{"date": "2026-09-28", "score": float64(3)},
			}
		}},
		{"bad history", func(s map[string]any, _ map[string]any) {
			s["streak"] = map[string]any{"makeup_dates": "2026-09-01"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			streak, heatmap := base()
			tc.edit(streak, heatmap)
			if _, err := rewardMakeupDate(streak, heatmap); err == nil {
				t.Fatal("ambiguous calendar must not yield a makeup date")
			}
		})
	}
}

func TestRewardConfigDefaultsAndAccountOverrides(t *testing.T) {
	cfg, err := parseRewardConfig(mustYAML(t, `
rewards:
  auto: true
  actions:
    lottery: false
  accounts:
    cn-abc:
      auto: false
      actions:
        makeup: false
`))
	if err != nil {
		t.Fatal(err)
	}
	base := resolveRewardSettings(cfg, "cn-other")
	if !base.Auto || base.Actions[rewardCheckin] != true || base.Actions[rewardLottery] != false {
		t.Fatalf("unexpected defaults: %#v", base)
	}
	override := resolveRewardSettings(cfg, "cn-abc")
	if override.Auto || override.Actions[rewardMakeup] || !override.Actions[rewardTravel] {
		t.Fatalf("unexpected override: %#v", override)
	}
}

func TestRewardConfigLegacyGrowthSwitchDoesNotDisableCheckin(t *testing.T) {
	cfg, err := parseRewardConfig(mustYAML(t, `
checkin_auto: true
growth_auto: false
`))
	if err != nil {
		t.Fatal(err)
	}
	settings := resolveRewardSettings(cfg, "cn-any")
	if !settings.Actions[rewardCheckin] {
		t.Fatal("checkin_auto must remain effective")
	}
	if settings.GrowthAuto == nil || *settings.GrowthAuto {
		t.Fatal("legacy growth_auto must disable growth-only actions")
	}
}

func TestRewardConfigRejectsUnknownActions(t *testing.T) {
	if _, err := parseRewardConfig(mustYAML(t, "rewards:\n  actions:\n    teleport: true\n")); err == nil {
		t.Fatal("unknown action must fail closed")
	}
}

func TestRewardConfigAccountKeyIsStableAndRegionScoped(t *testing.T) {
	a := rewardAccountKey("workbuddy", "cn", "uid-1", "ent", "auth-1")
	b := rewardAccountKey("workbuddy", "cn", "uid-1", "ent", "renamed-auth")
	if a != b {
		t.Fatalf("key changed with auth ID: %q != %q", a, b)
	}
	if a == rewardAccountKey("workbuddy", "global", "uid-1", "ent", "auth-1") {
		t.Fatal("region must be part of the identity")
	}
}

func TestNextRewardTimeUsesChinaWallClock(t *testing.T) {
	// 2026-10-20 16:00 UTC = 2026-10-21 00:00 China; next slot is 00:05 CST.
	now := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)
	next := nextRewardTime(now)
	want := time.Date(2026, 10, 21, 0, 5, 0, 0, rewardChinaZone)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	// 23:30 China must wrap to next day 00:05 China.
	late := time.Date(2026, 10, 21, 23, 30, 0, 0, rewardChinaZone)
	if got := nextRewardTime(late); !got.Equal(time.Date(2026, 10, 22, 0, 5, 0, 0, rewardChinaZone)) {
		t.Fatalf("late next = %s", got)
	}
}

func TestRewardRunnerNeverSendsGlobalCredentials(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	old := sharedClient
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requests++
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{}}`)), Request: req}, nil
	})}
	t.Cleanup(func() { sharedClient = old })

	files := []struct {
		name string
		sa   *storedAuth
	}{
		{"global-cb", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}}},
		{"global-wb", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}},
	}
	for _, tc := range files {
		t.Run(tc.name, func(t *testing.T) {
			if serviceFor(tc.sa) == serviceCN {
				t.Fatal("fixture must be international")
			}
			result, _ := runRewards(context.Background(), tc.sa, rewardActionOrder, true)
			if !result.Success || len(result.Steps) != 1 || result.Steps[0].Status != "skipped" {
				t.Fatalf("unexpected result: %#v", result)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("international accounts triggered %d upstream requests", requests)
	}
}

func TestRewardRunnerKeepsPartialCreditsAndFailures(t *testing.T) {
	call := func(_ context.Context, method, path string, _ any) (map[string]any, error) {
		switch path {
		case "/buddy/travel/status":
			return map[string]any{"state": "arrived", "record_id": float64(7), "daily_limit_reached": true}, nil
		case "/buddy/travel/claim":
			return map[string]any{"reward_credit": float64(10)}, nil
		case "/tasks":
			return nil, &rewardAPIError{Status: 500, Message: "task backend unavailable"}
		case "/streak":
			return nil, &rewardAPIError{Status: 500, Message: "streak backend unavailable"}
		}
		return map[string]any{}, nil
	}
	r := &rewardRunner{ctx: context.Background(), sa: &storedAuth{}, result: rewardResult{Success: true}, manual: true,
		callForTest: func(ctx context.Context, method, path string, body any) (map[string]any, error) {
			return call(ctx, method, path, body)
		}}
	r.travel()
	r.tasks()
	if r.result.Credits != 10 {
		t.Fatalf("credits = %d, want partial travel reward", r.result.Credits)
	}
	if r.result.Success || !r.result.NeedsAttention {
		t.Fatal("task failure must remain visible")
	}
}

func TestRewardRunnerDoesNotDepartAfterFailedTravelClaim(t *testing.T) {
	var paths []string
	call := func(_ context.Context, method, path string, _ any) (map[string]any, error) {
		paths = append(paths, path)
		if path == "/buddy/travel/status" {
			return map[string]any{"state": "arrived", "record_id": float64(9)}, nil
		}
		if path == "/buddy/travel/claim" {
			return nil, &rewardAPIError{Status: 500, Message: "claim failed"}
		}
		return map[string]any{"locations": []any{map[string]any{"id": float64(1)}}}, nil
	}
	r := &rewardRunner{ctx: context.Background(), sa: &storedAuth{}, result: rewardResult{Success: true},
		callForTest: func(ctx context.Context, method, path string, body any) (map[string]any, error) {
			return call(ctx, method, path, body)
		}}
	r.travel()
	for _, p := range paths {
		if p == "/buddy/travel/config" || p == "/buddy/travel/depart" {
			t.Fatalf("travel departed after failed claim: %v", paths)
		}
	}
}

func TestRewardRunnerPhysicalPrizeNeedsAttention(t *testing.T) {
	call := func(_ context.Context, method, path string, _ any) (map[string]any, error) {
		if path == "/lottery/chances" {
			return map[string]any{"balance": float64(2)}, nil
		}
		if path == "/lottery/draw" {
			return map[string]any{"prize_name": "杯子", "need_address": true}, nil
		}
		return nil, errors.New("unexpected")
	}
	r := &rewardRunner{ctx: context.Background(), sa: &storedAuth{}, result: rewardResult{Success: true},
		callForTest: func(ctx context.Context, method, path string, body any) (map[string]any, error) {
			return call(ctx, method, path, body)
		}}
	r.lottery()
	if !r.result.NeedsAttention {
		t.Fatal("physical prize must require address attention")
	}
	if len(r.result.Steps) != 1 || !strings.Contains(r.result.Steps[0].Message, "收件信息") {
		t.Fatalf("unexpected steps: %#v", r.result.Steps)
	}
}

func TestRewardRequestValidatesActions(t *testing.T) {
	_, _, err := parseRewardRequest([]byte(`{"actions":["checkin","nope"]}`), rewardActionOrder)
	if err == nil {
		t.Fatal("unknown action must be rejected")
	}
	_, actions, err := parseRewardRequest([]byte(`{"actions":["checkin","checkin"]}`), rewardActionOrder)
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %#v err=%v", actions, err)
	}
}

func TestRewardResultJSONShape(t *testing.T) {
	raw, err := json.Marshal(rewardResult{AuthIndex: "a", Steps: []rewardStep{{Action: "x", Status: "success"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"steps":[`) || !strings.Contains(string(raw), `"credits_gained":0`) {
		t.Fatalf("unexpected JSON: %s", raw)
	}
}

func mustYAML(t *testing.T, text string) *yaml.Node {
	t.Helper()
	node, err := parseValidatedConfigRoot([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func TestRewardRunDeduplicatesReservedAccount(t *testing.T) {
	const authIndex = "auth-dedupe"
	rewardState.Lock()
	oldRunning := rewardState.running
	oldResults := rewardState.results
	rewardState.running = map[string]bool{authIndex: true}
	rewardState.results = map[string]rewardResult{authIndex: {AuthIndex: authIndex, Running: true}}
	rewardState.Unlock()
	t.Cleanup(func() {
		rewardState.Lock()
		rewardState.running = oldRunning
		rewardState.results = oldResults
		rewardState.Unlock()
	})
	oldList := rewardHostAuthList
	rewardHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{AuthIndex: authIndex, ID: "id-1", Name: "workbuddy-uid.json"}}, nil
	}
	t.Cleanup(func() { rewardHostAuthList = oldList })
	oldGet := rewardHostAuthGet
	rewardHostAuthGet = func(string) (*storedAuth, error) {
		return &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}, Account: storedAccount{UID: "uid-1"}}, nil
	}
	t.Cleanup(func() { rewardHostAuthGet = oldGet })
	out := startRewardRun(authIndex, []rewardAction{rewardTravel}, false)
	if out["started"] != 0 || out["running"] != 1 {
		t.Fatalf("duplicate launch was not deduplicated: %#v", out)
	}
}

func TestRewardRunSkipsGlobalBeforeHttp(t *testing.T) {
	const authIndex = "auth-global"
	rewardState.Lock()
	oldResults := rewardState.results
	rewardState.results = map[string]rewardResult{}
	rewardState.Unlock()
	t.Cleanup(func() {
		rewardState.Lock()
		rewardState.results = oldResults
		rewardState.Unlock()
	})
	oldList := rewardHostAuthList
	rewardHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{AuthIndex: authIndex, ID: "id-intl", Name: "workbuddy-uid.json"}}, nil
	}
	t.Cleanup(func() { rewardHostAuthList = oldList })
	oldGet := rewardHostAuthGet
	rewardHostAuthGet = func(string) (*storedAuth, error) {
		return &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}, Account: storedAccount{UID: "uid-intl"}}, nil
	}
	t.Cleanup(func() { rewardHostAuthGet = oldGet })
	oldClient := sharedClient
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("unexpected HTTP request for international account: %s", req.URL)
		return nil, errors.New("unreachable")
	})}
	t.Cleanup(func() { sharedClient = oldClient })
	out := startRewardRun(authIndex, rewardActionOrder, true)
	if out["started"] != 0 || out["skipped"] != 1 {
		t.Fatalf("unexpected launch result: %#v", out)
	}
	rewardState.Lock()
	result := rewardState.results[authIndex]
	rewardState.Unlock()
	if len(result.Steps) != 1 || result.Steps[0].Status != "skipped" || result.NeedsAttention {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestLegacyCheckinSkipsGlobalBeforeProbe(t *testing.T) {
	const authIndex = "legacy-global"
	oldGet := checkinHostAuthGet
	checkinHostAuthGet = func(string) (*storedAuth, error) {
		return &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}}, nil
	}
	t.Cleanup(func() { checkinHostAuthGet = oldGet })
	oldClient := sharedClient
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("international /checkin made a request: %s", req.URL)
		return nil, errors.New("unreachable")
	})}
	t.Cleanup(func() { sharedClient = oldClient })
	out := checkinOneAccountWithCallback(pluginapi.HostAuthFileEntry{AuthIndex: authIndex, ID: "id"}, "")
	if out["reason"] != "global" || out["success"] != true {
		t.Fatalf("unexpected legacy result: %#v", out)
	}
}

func TestConfigureAppliesRewardOverrides(t *testing.T) {
	old := rewardConfigState.cfg
	oldCheckin, oldLifecycle := checkinAuto, lifecycleAuto
	t.Cleanup(func() {
		setRewardConfig(old)
		checkinAuto, lifecycleAuto = oldCheckin, oldLifecycle
	})
	raw := "rewards:\n  auto: false\n  actions:\n    lottery: false\n    makeup: false\n"
	if err := configure(mustJSON(map[string]any{"config_yaml": []byte(raw)})); err != nil {
		t.Fatal(err)
	}
	settings := rewardSettingsSnapshot()
	if settings.Auto || settings.Actions[rewardLottery] || settings.Actions[rewardMakeup] || !settings.Actions[rewardTravel] {
		t.Fatalf("unexpected applied settings: %#v", settings)
	}
}

func TestRewardRunHonoursDisabledAction(t *testing.T) {
	old := rewardConfigState.cfg
	oldCheckin, oldLifecycle := checkinAuto, lifecycleAuto
	t.Cleanup(func() {
		setRewardConfig(old)
		checkinAuto, lifecycleAuto = oldCheckin, oldLifecycle
	})
	setRewardConfig(rewardConfig{Defaults: rewardSettings{Auto: true, Actions: map[rewardAction]bool{
		rewardCheckin: true, rewardTravel: true, rewardTasks: true, rewardMakeup: true,
		rewardRedeem: true, rewardLottery: false, rewardEnergy: true,
	}}, Accounts: map[string]rewardOverride{}})
	settings := rewardSettingsSnapshot()
	selected := filterEnabledActions(rewardActionOrder, settings.Actions)
	for _, action := range selected {
		if action == rewardLottery {
			t.Fatal("disabled lottery must not be selected")
		}
	}
	if len(selected) != len(rewardActionOrder)-1 {
		t.Fatalf("selected = %v", selected)
	}
}

func TestRewardConfigAccountOverrideNarrowsOnlyThatAccount(t *testing.T) {
	cfg, err := parseRewardConfig(mustYAML(t, "rewards:\n  actions:\n    travel: true\n  accounts:\n    cn-x:\n      actions:\n        travel: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !resolveRewardSettings(cfg, "cn-y").Actions[rewardTravel] {
		t.Fatal("global travel setting must apply to other accounts")
	}
	if resolveRewardSettings(cfg, "cn-x").Actions[rewardTravel] {
		t.Fatal("account override must disable travel for that account")
	}
}

func TestRewardStatusHidesInternationalAccounts(t *testing.T) {
	oldList, oldGet := rewardHostAuthList, rewardHostAuthGet
	t.Cleanup(func() { rewardHostAuthList, rewardHostAuthGet = oldList, oldGet })
	rewardHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{AuthIndex: "cn", ID: "id-cn", Name: "workbuddy-cn.json"},
			{AuthIndex: "intl", ID: "id-intl", Name: "workbuddy-intl.json"},
		}, nil
	}
	rewardHostAuthGet = func(index string) (*storedAuth, error) {
		if index == "cn" {
			return &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}, Account: storedAccount{UID: "u-cn"}}, nil
		}
		return &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}, Account: storedAccount{UID: "u-intl"}}, nil
	}
	rewardState.Lock()
	oldResults := rewardState.results
	rewardState.results = map[string]rewardResult{
		"cn":   {AuthIndex: "cn", Success: true},
		"intl": {AuthIndex: "intl", Success: true},
	}
	rewardState.Unlock()
	t.Cleanup(func() { rewardState.Lock(); rewardState.results = oldResults; rewardState.Unlock() })
	status := handleRewardsStatus()
	rows, _ := status["results"].([]rewardResult)
	if len(rows) != 1 || rows[0].AuthIndex != "cn" {
		t.Fatalf("status exposed international results: %#v", rows)
	}
	accounts, _ := status["accounts"].(map[string]any)
	for key := range accounts {
		if strings.HasPrefix(key, "global-") {
			t.Fatalf("status exposed international account config: %s", key)
		}
	}
}

func TestRewardRunFiltersDisabledActionForManualRequest(t *testing.T) {
	oldCfg := rewardConfigState.cfg
	t.Cleanup(func() { setRewardConfig(oldCfg) })
	key := "cn-test"
	setRewardConfig(rewardConfig{
		Defaults: rewardSettings{Auto: true, Actions: map[rewardAction]bool{
			rewardCheckin: true, rewardTravel: true, rewardTasks: true, rewardMakeup: false,
			rewardRedeem: true, rewardLottery: true, rewardEnergy: true,
		}},
		Accounts: map[string]rewardOverride{key: {}},
	})
	settings := resolveRewardSettings(rewardConfigState.cfg, key)
	selected := filterEnabledActions([]rewardAction{rewardMakeup, rewardTravel}, settings.Actions)
	if len(selected) != 1 || selected[0] != rewardTravel {
		t.Fatalf("disabled action leaked into manual selection: %v", selected)
	}
}
