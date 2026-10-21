package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type testServerPaths struct {
	mu    sync.Mutex
	paths []string
}

func qoderTestServer(t *testing.T, handler http.HandlerFunc) *testServerPaths {
	t.Helper()
	server := &testServerPaths{}
	old := sharedHTTPClient()
	sharedClient = &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		server.mu.Lock()
		server.paths = append(server.paths, req.Method+" "+req.URL.Path)
		server.mu.Unlock()
		rec := &responseRecorder{status: 200, header: make(http.Header)}
		handler(rec, req)
		return &http.Response{StatusCode: rec.status, Header: rec.header, Body: io.NopCloser(strings.NewReader(rec.body)), Request: req}, nil
	})}
	t.Cleanup(func() { sharedClient = old })
	return server
}

func (s *testServerPaths) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

type responseRecorder struct {
	status int
	header http.Header
	body   string
}

func (r *responseRecorder) Header() http.Header    { return r.header }
func (r *responseRecorder) WriteHeader(status int) { r.status = status }
func (r *responseRecorder) Write(body []byte) (int, error) {
	r.body += string(body)
	return len(body), nil
}

func campaignBenefit(amount float64) struct {
	Amount float64 `json:"amount"`
} {
	return struct {
		Amount float64 `json:"amount"`
	}{Amount: amount}
}

func TestCampaignSummaryUsesWindowAndClaimStatus(t *testing.T) {
	now := time.Unix(1000, 0)
	rows := []rewardCampaign{
		{ID: "a", Key: "daily", Action: "CLAIM_BENEFIT", ClaimStatus: "CLAIMED", Benefit: campaignBenefit(100)},
		{ID: "b", Key: "future", Action: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", StartAt: 2000, Benefit: campaignBenefit(100)},
		{ID: "c", Key: "other", Action: "OTHER", ClaimStatus: "CLAIMABLE"},
	}
	summary := campaignSummary(rows, now)
	if !summary.Active || !summary.TodayCheckedIn || summary.TodayCredit != 100 || summary.DailyCredit != 0 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestCampaignClaimRequiresCLAIMEDStatus(t *testing.T) {
	server := qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"CLAIMED","benefit":{"amount":100}}}`))
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: "qoder.com.cn"}}
	rows := []rewardCampaign{{
		ID: "act-1", Key: "daily", Action: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE", Benefit: campaignBenefit(100),
	}}
	out, err := claimCampaigns(context.Background(), sa, rows)
	if err != nil {
		t.Fatal(err)
	}
	if out["success"] != true || out["rewardCredits"] != float64(100) {
		t.Fatalf("unexpected claim result: %#v", out)
	}
	if got := server.Paths(); len(got) != 1 || !strings.HasSuffix(got[0], "/act-1/claim") {
		t.Fatalf("paths = %v", got)
	}
}

func TestCampaignBusinessFailureIsReported(t *testing.T) {
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"PENDING"}}`))
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: "qoder.com.cn"}}
	out, err := claimCampaigns(context.Background(), sa, []rewardCampaign{{
		ID: "act-1", Action: "CLAIM_BENEFIT", ClaimStatus: "CLAIMABLE",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out["success"] != false || out["needs_attention"] != true {
		t.Fatalf("business failure was reported as success: %#v", out)
	}
}

func TestCampaignFallbackOnlyOnNotFound(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "campaigns") {
					w.WriteHeader(tc.status)
					return
				}
				_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			})
			sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Domain: "qoder.com.cn"}}
			_, err := fetchCheckinStatus(sa)
			paths := server.Paths()
			if tc.want {
				if err != nil {
					t.Fatalf("legacy fallback failed: %v", err)
				}
				if len(paths) < 2 || !strings.Contains(paths[len(paths)-1], "daily-check-in") {
					t.Fatalf("expected legacy fallback, paths=%v", paths)
				}
				return
			}
			if err == nil {
				t.Fatal("expected campaign error")
			}
			if len(paths) != 1 {
				t.Fatalf("must not fall back for %d: %v", tc.status, paths)
			}
		})
	}
}

func TestQoderRewardsSkipInternationalWithoutRequest(t *testing.T) {
	server := qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream request %s", r.URL)
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Region: "intl"}}
	steps, credits, success, attention, wrote := runQoderRewards(context.Background(), sa, rewardActionOrder, true)
	if !success || attention || wrote || credits != 0 || len(steps) != 1 || steps[0].Status != "skipped" {
		t.Fatalf("unexpected result: %#v %d %v %v %v", steps, credits, success, attention, wrote)
	}
	if len(server.Paths()) != 0 {
		t.Fatalf("international account made requests: %v", server.Paths())
	}
}

func TestQoderRewardConfigOverrides(t *testing.T) {
	root, err := parseQoderConfigRoot([]byte(`
checkin_auto: true
rewards:
  auto: true
  actions:
    checkin: false
  accounts:
    cn-xyz:
      auto: false
      actions:
        checkin: true
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseRewardConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Actions[rewardCheckin] || !cfg.Defaults.Auto {
		t.Fatalf("defaults: %#v", cfg.Defaults)
	}
	override, ok := cfg.Accounts["cn-xyz"]
	if !ok || override.Auto == nil || *override.Auto || override.Actions[rewardCheckin] == nil || !*override.Actions[rewardCheckin] {
		t.Fatalf("override: %#v", override)
	}
}

func TestNextRewardTimeQoderUsesBeijingTime(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) // 08:00 China
	got := nextRewardTime(now)
	want := time.Date(2026, 10, 21, 10, 5, 0, 0, campaignZone)
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCampaignJSONKeepsExplicitClaim(t *testing.T) {
	var rows []rewardCampaign
	if err := json.Unmarshal([]byte(`[{"campaignId":"a","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}]`), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ID != "a" || rows[0].Benefit.Amount != 100 {
		t.Fatalf("rows=%#v", rows)
	}
}

func TestLegacyCheckinSkipsInternationalBeforeRequest(t *testing.T) {
	server := qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("international legacy check-in made a request: %s", r.URL)
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "dt-test", Region: "intl"}}
	steps, _, success, attention, wrote := runQoderRewards(context.Background(), sa, rewardActionOrder, true)
	if !success || attention || wrote || len(steps) != 1 || steps[0].Status != "skipped" {
		t.Fatalf("unexpected Qoder legacy result: %#v", steps)
	}
	if len(server.Paths()) != 0 {
		t.Fatalf("international legacy check-in issued requests: %v", server.Paths())
	}
}

func TestConfigureAppliesQoderRewardSettings(t *testing.T) {
	old := rewardConfigState.cfg
	oldCheckin, oldLifecycle, oldScheduler := checkinAuto, lifecycleAuto, schedulerMode
	oldKeepalive, oldURL, oldKey := keepaliveAuto, usageReportURL, usageReportKey
	oldManagement := managementAPIKey
	t.Cleanup(func() {
		setRewardConfig(old)
		checkinAuto, lifecycleAuto, schedulerMode = oldCheckin, oldLifecycle, oldScheduler
		keepaliveAuto, usageReportURL, usageReportKey = oldKeepalive, oldURL, oldKey
		managementAPIKey = oldManagement
	})
	raw := "rewards:\n  auto: false\n  actions:\n    checkin: false\n"
	configure(mustJSONForTest(map[string]any{"config_yaml": []byte(raw)}))
	settings := rewardConfigSnapshot()
	if settings.Auto || settings.Actions[rewardCheckin] {
		t.Fatalf("reward settings were not applied: %#v", settings)
	}
}

func mustJSONForTest(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func TestQoderRewardRequestHonoursDisabledAction(t *testing.T) {
	old := rewardConfigState.cfg
	oldCheckin, oldLifecycle := checkinAuto, lifecycleAuto
	t.Cleanup(func() {
		setRewardConfig(old)
		checkinAuto, lifecycleAuto = oldCheckin, oldLifecycle
	})
	setRewardConfig(rewardConfig{Defaults: rewardSettings{Auto: true, Actions: map[rewardAction]bool{rewardCheckin: false}}, Accounts: map[string]rewardOverride{}})
	if selected := filterEnabledActions(rewardActionOrder, rewardConfigSnapshot().Actions); len(selected) != 0 {
		t.Fatalf("disabled checkin was selected: %v", selected)
	}
}

func TestQoderRewardStatusHidesInternationalResults(t *testing.T) {
	oldList, oldGet := rewardHostAuthList, rewardHostAuthGet
	t.Cleanup(func() { rewardHostAuthList, rewardHostAuthGet = oldList, oldGet })
	rewardHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{AuthIndex: "cn", ID: "id-cn", Name: "qoderwork-cn.json"},
			{AuthIndex: "intl", ID: "id-intl", Name: "qoderwork-intl.json"},
		}, nil
	}
	rewardHostAuthGet = func(index string) (*storedAuth, error) {
		if index == "cn" {
			return &storedAuth{Auth: storedTokens{Domain: "qoder.com.cn"}, Account: storedAccount{UID: "u-cn"}}, nil
		}
		return &storedAuth{Auth: storedTokens{Region: "intl"}, Account: storedAccount{UID: "u-intl"}}, nil
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
		if strings.HasPrefix(key, "intl-") {
			t.Fatalf("status exposed international account config: %s", key)
		}
	}
}
