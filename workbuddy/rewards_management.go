package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const rewardRunBudget = 180 * time.Second

// Seams used by tests to drive the CN gate without a host RPC.
var rewardHostAuthList = hostAuthList
var rewardHostAuthGet = hostAuthGet

var rewardState = struct {
	sync.Mutex
	running map[string]bool
	results map[string]rewardResult
}{
	running: map[string]bool{},
	results: map[string]rewardResult{},
}

var rewardAccountSlots = make(chan struct{}, 4)

func rewardActionNames(actions []rewardAction) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, string(action))
	}
	return out
}

func parseRewardRequest(body []byte, defaults []rewardAction) (authIndex string, actions []rewardAction, err error) {
	var req struct {
		AuthIndex string   `json:"auth_index"`
		Actions   []string `json:"actions"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return "", nil, fmt.Errorf("invalid JSON")
		}
	}
	authIndex = strings.TrimSpace(req.AuthIndex)
	if len(req.Actions) == 0 {
		return authIndex, append([]rewardAction(nil), defaults...), nil
	}
	seen := map[rewardAction]bool{}
	for _, raw := range req.Actions {
		action, ok := validRewardAction(raw)
		if !ok {
			return "", nil, fmt.Errorf("unknown reward action %q", raw)
		}
		if !seen[action] {
			seen[action] = true
			actions = append(actions, action)
		}
	}
	return authIndex, actions, nil
}

func handleRewardsRun(req pluginapi.ManagementRequest) map[string]any {
	authIndex, actions, err := parseRewardRequest(req.Body, rewardActionOrder)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return startRewardRun(authIndex, actions, false)
}

func handleRewardsStatus() map[string]any {
	files, err := rewardHostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	live := map[string]bool{}
	for _, f := range files {
		live[f.AuthIndex] = true
	}
	// Snapshot results under the lock, then do host RPCs outside it: a slow
	// host.auth.get must not block a reward run from recording progress.
	rewardState.Lock()
	snapshot := make(map[string]rewardResult, len(rewardState.results))
	for idx, result := range rewardState.results {
		if !live[idx] && !result.Running {
			delete(rewardState.results, idx)
			delete(rewardState.running, idx)
			continue
		}
		snapshot[idx] = result
	}
	rewardState.Unlock()

	defaults := rewardSettingsSnapshot()
	results := make([]rewardResult, 0, len(snapshot))
	accounts := map[string]any{}
	configAccounts := rewardConfigJSON()["accounts"]
	for _, f := range files {
		sa, err := rewardHostAuthGet(f.AuthIndex)
		if err != nil || serviceFor(sa) != serviceCN {
			// Status and overrides are a CN reward surface. International
			// accounts remain absent, including stale draft results.
			continue
		}
		if result, ok := snapshot[f.AuthIndex]; ok {
			results = append(results, result)
		}
		accounts[workbuddyRewardKey(sa, f.ID)] = map[string]any{
			"auth_index": f.AuthIndex, "name": f.Name, "nickname": sa.Account.Nickname,
			"region": accountRegion(sa), "settings": rewardSettingsFor(sa, f.ID),
		}
	}
	return map[string]any{
		"auto":            defaults.Auto,
		"actions":         defaults.Actions,
		"accounts":        accounts,
		"config_accounts": configAccounts,
		"schedule":        []string{"00:05", "01:05", "05:05", "09:05", "13:05", "17:05", "21:05"},
		"timezone":        "Asia/Shanghai",
		"results":         results,
		"server_time":     time.Now().Format("2006-01-02 15:04:05"),
	}
}

// startRewardRun filters to CN accounts before reserving work. An explicit
// international auth_index returns a local skip and never reads credentials.
func startRewardRun(authIndex string, actions []rewardAction, automatic bool) map[string]any {
	files, err := rewardHostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	started, running, matched, skipped := 0, 0, 0, 0
	for _, f := range files {
		if authIndex != "" && f.AuthIndex != authIndex {
			continue
		}
		matched++
		sa, err := rewardHostAuthGet(f.AuthIndex)
		if err != nil {
			rewardState.Lock()
			rewardState.results[f.AuthIndex] = rewardResult{
				AuthIndex: f.AuthIndex, AuthID: f.ID, Name: f.Name, Success: false, NeedsAttention: true,
				Steps: []rewardStep{{Action: "读取账号", Status: "error", Message: "账号凭据读取失败"}},
			}
			rewardState.Unlock()
			continue
		}
		// Hard region gate: do not probe, claim, refresh quota, or write auth.
		if serviceFor(sa) != serviceCN {
			skipped++
			rewardState.Lock()
			rewardState.results[f.AuthIndex] = rewardResult{
				AuthIndex: f.AuthIndex, AuthID: f.ID, Name: f.Name, Nickname: sa.Account.Nickname,
				Success: true, Steps: []rewardStep{{Action: "奖励领取", Status: "skipped", Message: "国际版账号不参与签到与成长奖励"}},
				FinishedAt: time.Now().Format(time.RFC3339),
			}
			rewardState.Unlock()
			continue
		}
		settings := rewardSettingsFor(sa, f.ID)
		// The panel and scheduler both honor configured switches. A disabled
		// resource-consuming action must not run even from a one-click button.
		selected := filterEnabledActions(actions, settings.Actions)
		if automatic && settings.GrowthAuto != nil && !*settings.GrowthAuto {
			for _, action := range selected {
				if action != rewardCheckin {
					settings.Actions[action] = false
				}
			}
			selected = filterEnabledActions(actions, settings.Actions)
		}
		if automatic && !settings.Auto {
			// The global auto switch gates growth actions. The legacy
			// check-in switch remains independent, so a CN account with
			// check-in enabled still gets its daily claim.
			if !settings.Actions[rewardCheckin] {
				skipped++
				continue
			}
			selected = []rewardAction{rewardCheckin}
		}
		if len(selected) == 0 {
			skipped++
			continue
		}
		rewardState.Lock()
		if rewardState.running[f.AuthIndex] {
			running++
			rewardState.Unlock()
			continue
		}
		rewardState.running[f.AuthIndex] = true
		rewardState.results[f.AuthIndex] = rewardResult{
			AuthIndex: f.AuthIndex, AuthID: f.ID, Name: f.Name, Nickname: sa.Account.Nickname,
			UID: sa.Account.UID, Actions: rewardActionNames(selected), Running: true, Success: true,
			StartedAt: time.Now().Format(time.RFC3339),
			Steps:     []rewardStep{{Action: "奖励领取", Status: "running", Message: "正在执行"}},
		}
		rewardState.Unlock()
		started++
		go executeRewardAccount(f, sa, selected, automatic)
	}
	if authIndex != "" && matched == 0 {
		return map[string]any{"error": "account not found"}
	}
	return map[string]any{"started": started, "running": running, "skipped": skipped}
}

func filterEnabledActions(actions []rewardAction, enabled map[rewardAction]bool) []rewardAction {
	out := make([]rewardAction, 0, len(actions))
	for _, action := range actions {
		if enabled[action] {
			out = append(out, action)
		}
	}
	return out
}

func executeRewardAccount(f pluginapi.HostAuthFileEntry, sa *storedAuth, actions []rewardAction, automatic bool) {
	rewardAccountSlots <- struct{}{}
	defer func() { <-rewardAccountSlots }()
	result := rewardResult{AuthIndex: f.AuthIndex, AuthID: f.ID, Name: f.Name, Nickname: sa.Account.Nickname, UID: sa.Account.UID, Success: true}
	withCheckinLock(f.AuthIndex, func() {
		// Re-check region after acquiring the lock: credentials can be replaced
		// while a run is queued. Never let a stale CN snapshot reach upstream.
		fresh, err := rewardHostAuthGet(f.AuthIndex)
		if err != nil {
			result.Success = false
			result.NeedsAttention = true
			result.Steps = append(result.Steps, rewardStep{Action: "读取账号", Status: "error", Message: "账号凭据读取失败"})
			return
		}
		if serviceFor(fresh) != serviceCN {
			result.Steps = append(result.Steps, rewardStep{Action: "奖励领取", Status: "skipped", Message: "国际版账号不参与签到与成长奖励"})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), rewardRunBudget)
		defer cancel()
		runner, wrote := runRewards(ctx, fresh, actions, !automatic)
		result.Actions = rewardActionNames(actions)
		result.Success = runner.Success
		result.NeedsAttention = runner.NeedsAttention
		result.Credits = runner.Credits
		result.Steps = runner.Steps
		// Only a CN account that actually wrote a reward is refreshed. The
		// refresh is the existing CN lifecycle path; international accounts are
		// never included in a bulk reconcile from the reward scheduler.
		if wrote {
			accountCache.Delete(f.ID)
			if lifecycleEnabled() {
				_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
			}
		}
	})
	if result.Steps == nil {
		result.Steps = []rewardStep{}
	}
	result.Running = false
	result.FinishedAt = time.Now().Format(time.RFC3339)
	rewardState.Lock()
	delete(rewardState.running, f.AuthIndex)
	rewardState.results[f.AuthIndex] = result
	rewardState.Unlock()
}

// runAutomaticRewards is the scheduled entry. It filters CN accounts inside
// startRewardRun; it never calls a bulk lifecycle reconcile.
func runAutomaticRewards() map[string]any {
	if !rewardSettingsSnapshot().Auto {
		return map[string]any{"started": 0, "disabled": true}
	}
	return startRewardRun("", rewardActionOrder, true)
}

// runScheduledCheckinAndRewards keeps the existing /checkin-compatible
// automatic behavior while adding the full reward pass. Both paths use the
// same per-account lock and CN gate.
func runScheduledCheckinAndRewards() {
	// The reward pass already performs the daily check-in action, so a second
	// check-in probe would only add duplicate upstream reads.
	runAutomaticRewards()
	if !lifecycleEnabled() {
		return
	}
	files, err := rewardHostAuthList()
	if err != nil {
		return
	}
	for _, f := range files {
		sa, err := rewardHostAuthGet(f.AuthIndex)
		if err != nil || serviceFor(sa) != serviceCN {
			continue
		}
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

var rewardChinaZone = time.FixedZone("Asia/Shanghai", 8*60*60)

// nextRewardTime uses China wall-clock hours regardless of the host timezone.
func nextRewardTime(now time.Time) time.Time {
	local := now.In(rewardChinaZone)
	for _, hour := range []int{0, 1, 5, 9, 13, 17, 21} {
		next := time.Date(local.Year(), local.Month(), local.Day(), hour, 5, 0, 0, rewardChinaZone)
		if next.After(now) {
			return next
		}
	}
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 5, 0, 0, rewardChinaZone)
}

func rewardSchedulerLoop(stop chan struct{}) {
	// Initial catch-up waits for host auth initialization. It is deliberately
	// a single delayed run, not a tick every 30s.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			runScheduledCheckinAndRewards()
			timer.Reset(time.Until(nextRewardTime(time.Now())))
		}
	}
}
