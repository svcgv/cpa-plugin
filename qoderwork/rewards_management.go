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

func parseRewardRequest(body []byte, defaults []rewardAction) (string, []rewardAction, error) {
	var req struct {
		AuthIndex string   `json:"auth_index"`
		Actions   []string `json:"actions"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return "", nil, fmt.Errorf("invalid JSON")
		}
	}
	authIndex := strings.TrimSpace(req.AuthIndex)
	if len(req.Actions) == 0 {
		return authIndex, append([]rewardAction(nil), defaults...), nil
	}
	seen := map[rewardAction]bool{}
	actions := make([]rewardAction, 0, len(req.Actions))
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

func rewardActionNames(actions []rewardAction) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, string(action))
	}
	return out
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
	rewardState.Lock()
	snapshot := make(map[string]rewardResult, len(rewardState.results))
	for index, result := range rewardState.results {
		if !live[index] && !result.Running {
			delete(rewardState.results, index)
			delete(rewardState.running, index)
			continue
		}
		snapshot[index] = result
	}
	rewardState.Unlock()

	defaults := rewardConfigSnapshot()
	results := make([]rewardResult, 0, len(snapshot))
	accounts := map[string]any{}
	configAccounts := rewardConfigJSON()["accounts"]
	for _, f := range files {
		sa, err := rewardHostAuthGet(f.AuthIndex)
		if err != nil || !sa.isCN() {
			continue
		}
		if result, ok := snapshot[f.AuthIndex]; ok {
			results = append(results, result)
		}
		accounts[qoderRewardKey(sa, f.ID)] = map[string]any{
			"auth_index": f.AuthIndex, "name": f.Name, "nickname": sa.Account.Nickname,
			"region": string(sa.region()), "settings": rewardSettingsFor(sa, f.ID),
		}
	}
	return map[string]any{
		"auto": defaults.Auto, "actions": defaults.Actions,
		"schedule": []string{"10:05", "11:05", "15:05", "19:05", "23:05"},
		"timezone": "Asia/Shanghai", "accounts": accounts,
		"config_accounts": configAccounts,
		"results":         results, "server_time": time.Now().Format("2006-01-02 15:04:05"),
	}
}

// startRewardRun filters to CN accounts before any reservation or write.
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
		if !sa.isCN() {
			skipped++
			rewardState.Lock()
			rewardState.results[f.AuthIndex] = rewardResult{
				AuthIndex: f.AuthIndex, AuthID: f.ID, Name: f.Name, Nickname: sa.Account.Nickname,
				Success: true, Steps: []rewardStep{{Action: "奖励领取", Status: "skipped", Message: "国际版账号不参与签到与每日奖励"}},
				FinishedAt: time.Now().Format(time.RFC3339),
			}
			rewardState.Unlock()
			continue
		}
		settings := rewardSettingsFor(sa, f.ID)
		// The scheduler and the panel both respect the configured check-in
		// switch; manual buttons cannot bypass a disabled action.
		selected := filterEnabledActions(actions, settings.Actions)
		if automatic && !settings.Auto {
			// Keep the legacy checkin_auto behavior independent from the
			// reward-wide auto switch.
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
	result := rewardResult{
		AuthIndex: f.AuthIndex, AuthID: f.ID, Name: f.Name,
		Nickname: sa.Account.Nickname, UID: sa.Account.UID, Success: true,
		Actions: rewardActionNames(actions),
	}
	func() {
		mu := checkinLockFor(f.AuthIndex)
		mu.Lock()
		defer mu.Unlock()
		fresh, err := rewardHostAuthGet(f.AuthIndex)
		if err != nil {
			result.Success = false
			result.NeedsAttention = true
			result.Steps = append(result.Steps, rewardStep{Action: "读取账号", Status: "error", Message: "账号凭据读取失败"})
			return
		}
		if !fresh.isCN() {
			result.Steps = append(result.Steps, rewardStep{Action: "奖励领取", Status: "skipped", Message: "国际版账号不参与签到与每日奖励"})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), rewardRunBudget)
		defer cancel()
		steps, credits, success, attention, wrote := runQoderRewards(ctx, fresh, actions, !automatic)
		result.Credits = credits
		result.Success = success
		result.NeedsAttention = attention
		result.Steps = steps
		if wrote {
			accountCache.Delete(f.ID)
			if lifecycleEnabled() {
				_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
			}
		}
	}()
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

// runQoderRewards is the only reward write path used by both the scheduler and
// the management endpoints. A Global check occurs before campaign fetching.
func runQoderRewards(ctx context.Context, sa *storedAuth, actions []rewardAction, manual bool) (steps []rewardStep, credits int64, success, attention, wrote bool) {
	success = true
	if sa == nil || !sa.isCN() {
		return []rewardStep{{Action: "奖励领取", Status: "skipped", Message: "国际版账号不参与签到与每日奖励"}}, 0, true, false, false
	}
	for _, action := range actions {
		if action != rewardCheckin {
			continue
		}
		if err := ctx.Err(); err != nil {
			steps = append(steps, rewardStep{Action: string(action), Status: "error", Message: "本轮时间预算耗尽，剩余项下轮继续"})
			success = false
			attention = true
			continue
		}
		res, err := performCheckinCall(sa)
		if err != nil {
			steps = append(steps, rewardStep{Action: string(action), Status: "error", Message: truncateRedacted(err.Error(), 200)})
			success = false
			attention = true
			continue
		}
		if res["rewardCredits"] != nil {
			switch value := res["rewardCredits"].(type) {
			case float64:
				credits += int64(value)
			case int64:
				credits += value
			}
		}
		msg, _ := res["message"].(string)
		if msg == "" {
			msg = "每日奖励领取完成"
		}
		status := "success"
		if res["skipped"] == true {
			status = "skipped"
		}
		if res["success"] != true {
			status = "error"
			success = false
			attention = true
		}
		steps = append(steps, rewardStep{Action: string(action), Status: status, Message: msg})
		if claimed, ok := res["campaigns"].([]string); ok && len(claimed) > 0 {
			wrote = true
		}
	}
	return
}

func runAutomaticRewards() map[string]any {
	if !rewardConfigSnapshot().Auto {
		return map[string]any{"started": 0, "disabled": true}
	}
	return startRewardRun("", rewardActionOrder, true)
}

func runScheduledCheckinAndRewards() {
	// The reward pass covers the daily campaign claim. Reconcile CN accounts
	// afterwards so credit exhaustion/restoration still updates lifecycle.
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
		if err != nil || !sa.isCN() {
			continue
		}
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

var campaignZone = time.FixedZone("Asia/Shanghai", 8*60*60)

func nextRewardTime(now time.Time) time.Time {
	cn := now.In(campaignZone)
	for _, hour := range campaignHours {
		next := time.Date(cn.Year(), cn.Month(), cn.Day(), hour, 5, 0, 0, campaignZone)
		if next.After(now) {
			return next
		}
	}
	return time.Date(cn.Year(), cn.Month(), cn.Day()+1, 10, 5, 0, 0, campaignZone)
}

func rewardSchedulerLoop(stop chan struct{}) {
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
