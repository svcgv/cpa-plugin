// checkin.go keeps the legacy /checkin entry and token keepalive scheduler.
// CN campaigns run through rewards_management.go in China time; international
// accounts are rejected locally before any reward request.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
	go rewardSchedulerLoop(schedulerStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

// nextKeepaliveWakeTime returns the next local 22:00 token refresh. Campaign
// rewards are scheduled exclusively by rewardSchedulerLoop in China time.
func nextKeepaliveWakeTime(now time.Time) time.Time {
	var earliest time.Time
	for _, hour := range keepaliveHours {
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	return earliest
}

func schedulerLoop(stop chan struct{}) {
	for {
		next := nextKeepaliveWakeTime(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			// The legacy loop only handles token keepalive; reward work has its
			// own China-time scheduler and must not be triggered twice.
			if keepaliveEnabled() && shouldRunKeepaliveNow(time.Now()) {
				runTokenKeepalive()
			}
		}
	}
}

// runAutoCheckin is the scheduled lifecycle tick (09:00 / 21:00).
// CN: optional daily check-in, then reconcile (disable exhausted / reenable after credits).
// CN: no auto trial (one-shot claim is manual only); reconcile may delete exhausted auths.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
// handleManualCheckin checks in one account (auth_index) or all qoderwork
// accounts. Unlike the workbuddy three-phase classify/execute/summarize flow,
// QoderWork's checkin is a simple Bearer GET+POST — we run it directly per
// account under an 8s timeout, no classify stage.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	t0 := time.Now()
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	// Run checkins concurrently (one goroutine per account, bounded).
	type result struct {
		idx int
		out map[string]any
	}
	outCh := make(chan result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outCh <- result{idx: i, out: checkinOneAccount(f)}
		}(i, f)
	}
	wg.Wait()
	close(outCh)

	results := make([]map[string]any, len(targets))
	for r := range outCh {
		results[r.idx] = r.out
	}
	successN, alreadyN, failN := 0, 0, 0
	for _, r := range results {
		if r["error"] != nil {
			failN++
			continue
		}
		if r["skipped"] == true {
			alreadyN++
			continue
		}
		if r["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":      len(targets),
			"success":    successN,
			"already":    alreadyN,
			"fail":       failN,
			"elapsed_ms": time.Since(t0).Milliseconds(),
		},
	}
}

// checkinOneAccount performs the actual GET status + POST claim flow for one
// account. 8s overall budget — never blocks past that.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	out := map[string]any{"auth_index": f.AuthIndex, "name": f.Name}
	sa, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		out["error"] = "get auth: " + err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname
	if !sa.isCN() {
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "intl"
		out["message"] = "国际版不参与签到与每日奖励"
		return out
	}
	mu := checkinLockFor(f.AuthIndex)
	mu.Lock()
	defer mu.Unlock()
	steps, credits, success, attention, _ := runQoderRewards(context.Background(), sa, rewardActionOrder, true)
	out["success"] = success
	out["needs_attention"] = attention
	if credits > 0 {
		out["reward_credits"] = credits
	}
	if len(steps) > 0 {
		out["message"] = steps[len(steps)-1].Message
		if steps[len(steps)-1].Status == "skipped" {
			out["skipped"] = true
			out["reason"] = "already"
		}
	}
	if attention && success == false {
		out["error"] = out["message"]
	}
	return out
}

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// checkProUpgradeEligibility returns whether the account can still claim the
// one-time Pro Upgrade pack (+1800).
func checkProUpgradeEligibility(sa *storedAuth) (bool, error) {
	if !sa.isCN() {
		return false, errCheckinUnsupported
	}
	req, err := http.NewRequest(http.MethodGet, upstreamBaseCN+"/sash/api/v1/me/pro-upgrade/eligibility", nil)
	if err != nil {
		return false, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("http %d", resp.StatusCode)
	}
	var m struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return false, err
	}
	return m.Eligible, nil
}

// claimProUpgrade claims the one-time Pro Upgrade pack (+1800 credits).
func claimProUpgrade(sa *storedAuth) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, endpointProUpgrade, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"success": false, "message": fmt.Sprintf("http %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, err
	}
	if _, ok := m["success"]; !ok {
		m["success"] = true
	}
	return m, nil
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}

// handleClaimPro checks pro-upgrade eligibility for one account and claims
// the one-time Pro pack (+ credits) when eligible. Surfaced as a panel
// per-card button — NOT called automatically during login (login writes the
// auth file first; this is a user-triggered post-login action).
func handleClaimPro(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		return map[string]any{"error": "get auth: " + err.Error()}
	}
	out := map[string]any{
		"auth_index": authIndex,
		"nickname":   sa.Account.Nickname,
	}
	if !sa.isCN() {
		out["success"] = false
		out["message"] = "国际版无 Pro 升级包（仅国内版 qoder.com.cn 提供）"
		return out
	}
	eligible, err := checkProUpgradeEligibility(sa)
	if err != nil {
		out["error"] = "eligibility: " + err.Error()
		return out
	}
	if !eligible {
		out["success"] = false
		out["message"] = "不可领取（已领或活动未开放）"
		return out
	}
	res, err := claimProUpgrade(sa)
	if err != nil {
		out["error"] = "claim: " + err.Error()
		return out
	}
	for k, v := range res {
		out[k] = v
	}
	if _, ok := out["success"]; !ok {
		out["success"] = true
	}
	// Refresh credits snapshot so panel shows updated balance immediately.
	if cr, crErr := fetchUserResource(sa); crErr == nil && cr != nil {
		out["credits"] = cr
	}
	return out
}
