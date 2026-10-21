// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the 09:00 / 21:00 auto scheduler, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. Global accounts are excluded — they use one-shot trial claims instead.
package main

import (
	"encoding/json"
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

func scheduledInCurrentHour(now time.Time, hours []int) bool {
	for _, h := range hours {
		start := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !now.Before(start) && now.Before(start.Add(time.Hour)) {
			return true
		}
	}
	return false
}

func scheduledActionsFor(now time.Time) (runCheckin, runKeepalive bool) {
	// The live scheduler uses rewardSchedulerLoop for China-time rewards and
	// only calls this helper for stale-call compatibility.
	return false, scheduledInCurrentHour(now, keepaliveHours)
}

// nextKeepaliveTime is defined in keepalive.go; check-in and growth are
// scheduled only by rewardSchedulerLoop in China time.
func schedulerLoop(stop chan struct{}) {
	for {
		timer := time.NewTimer(time.Until(nextKeepaliveTime(time.Now())))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			if keepaliveEnabled() && shouldRunKeepaliveNow(time.Now()) {
				runTokenKeepalive()
			}
		}
	}
}

// handleManualCheckin serves POST /checkin.
//
// Single-account mode (body.auth_index set): runs checkinOneAccount directly —
// one hostAuthGet + at most two upstream calls (status, then check-in). The
// old three-phase classify/execute/summarize pipeline ran the same framework
// for one account as for thirty (4 RPC + 5 HTTP + lock-held re-reads), which
// routinely blew past the 30s management API timeout: the check-in succeeded
// upstream but the response never reached the panel.
//
// Batch mode (empty auth_index): fans out checkinOneAccount per account
// (sem=4), preserving input order and the {results, summary} response shape
// the panel depends on.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	return handleManualCheckinWithCallback(req, "")
}

func handleManualCheckinWithCallback(req pluginapi.ManagementRequest, callbackID string) map[string]any {
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

	results := make([]map[string]any, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = checkinOneAccountWithCallback(f, callbackID)
		}(i, f)
	}
	wg.Wait()

	// Summary counters (field names are a panel contract — panel.html
	// checkinAll reads success/already/fail/skipped_global/eligible).
	successN, failN, alreadyN, globalN, eligibleN := 0, 0, 0, 0, 0
	unsupportedN, unknownN := 0, 0
	for _, out := range results {
		if out == nil {
			continue
		}
		if out["error"] != nil {
			failN++
			continue
		}
		reason, _ := out["reason"].(string)
		switch reason {
		case "already":
			alreadyN++
			continue
		case "unsupported":
			// The account has no daily check-in activity. Counting this as a
			// failure would make "全部签到" look broken for an international
			// account that simply has nothing to claim.
			unsupportedN++
			continue
		case "unknown":
			unknownN++
			continue
		case "global":
			// Legacy reason kept for results produced by older flows.
			globalN++
			continue
		}
		eligibleN++
		if out["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":          len(targets),
			"eligible":       eligibleN,
			"success":        successN,
			"already":        alreadyN,
			"unsupported":    unsupportedN,
			"unknown":        unknownN,
			"skipped_global": globalN,
			"fail":           failN,
			"attempted":      eligibleN,
		},
	}
}

// checkinOneAccount performs the daily check-in for one account with the
// minimum number of round-trips:
//
//	hostAuthGet ×1 (RPC) → domain gate → fetchCheckinStatus ×1 (failure
//	tolerated, upstream is idempotent) → performCheckinCall ×1.
//
// No lock-held re-reads, no lifecycle reconcile (that is the scheduled
// runAutoCheckin path's job), no redundant status refetches. Cache updates
// merge into the previous entry so credits/plan survive.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	return checkinOneAccountWithCallback(f, "")
}

// checkinHostAuthGet fetches one credential for the check-in path. Overridable
// so tests can drive the full decision path without a host bridge.
var checkinHostAuthGet = hostAuthGet

func checkinOneAccountWithCallback(f pluginapi.HostAuthFileEntry, callbackID string) map[string]any {
	out := map[string]any{"auth_index": f.AuthIndex}
	sa, err := checkinHostAuthGet(f.AuthIndex)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname
	// Location is checked before probing or mutating anything. A Global
	// credential is a local skip, not an activity-capability error.
	if serviceFor(sa) != serviceCN {
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "global"
		out["message"] = "国际版账号不参与签到与成长奖励"
		return out
	}

	withCheckinLock(f.AuthIndex, func() {
		probe := checkinCapabilityProbe(sa, callbackID)
		if probe.Summary != nil {
			probe.Summary.Capability = string(probe.Capability)
			probe.Summary.CapabilityNote = probe.Message
		}
		switch probe.Capability {
		case capabilityUnsupported:
			mergeCheckinCache(f.ID, &checkinSummary{
				Capability: string(capabilityUnsupported), CapabilityNote: probe.Message,
			})
			out["success"] = false
			out["skipped"] = true
			out["reason"] = "unsupported"
			out["message"] = probe.Message
			return
		case capabilityUnknown:
			if probe.Summary != nil {
				mergeCheckinCache(f.ID, probe.Summary)
			}
		default:
			if probe.Summary != nil && probe.Summary.TodayCheckedIn {
				mergeCheckinCache(f.ID, probe.Summary)
				out["success"] = true
				out["skipped"] = true
				out["reason"] = "already"
				out["message"] = "今日已签到"
				return
			}
			if probe.Summary != nil {
				mergeCheckinCache(f.ID, probe.Summary)
			}
		}

		res, err := performCheckinCallWithCallback(sa, callbackID)
		if err != nil {
			out["error"] = err.Error()
			out["success"] = false
			return
		}
		for k, v := range res {
			out[k] = v
		}
		if msg, _ := out["message"].(string); msg != "" && out["success"] == false {
			low := strings.ToLower(msg)
			if strings.Contains(low, "already") || strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
				out["success"] = true
				out["skipped"] = true
				out["reason"] = "already"
			}
		}
		if _, ok := out["success"]; !ok {
			out["success"] = true
		}
		if out["success"] != true {
			if out["message"] == nil {
				out["message"] = "签到未成功"
			}
			return
		}
		if ci2, err2 := fetchCheckinStatusWithCallback(sa, callbackID); err2 == nil && ci2 != nil {
			mergeCheckinCache(f.ID, ci2)
		} else {
			mergeCheckinCache(f.ID, &checkinSummary{TodayCheckedIn: true})
		}
	})
	return out
}

// mergeCheckinCache stores the latest check-in snapshot while preserving the
// credits/plan fields of any previous cache entry (merge, not replace).
func mergeCheckinCache(authID string, ci *checkinSummary) {
	var prev *accountCacheEntry
	if v, ok := accountCache.Load(authID); ok {
		prev, _ = v.(*accountCacheEntry)
	}
	entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
	if prev != nil {
		entry.credits = prev.credits
		entry.plan = prev.plan
	}
	accountCache.Store(authID, entry)
}

func withCheckinLock(authIndex string, fn func()) {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()
	fn()
}

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
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
