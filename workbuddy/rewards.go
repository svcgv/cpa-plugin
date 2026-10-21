// CN reward collection for WorkBuddy. Protocol adapted from
// 88lin/workbuddy-auto-signin (see ../THIRD_PARTY_NOTICES.md).
//
// Every action is gated on a CN service before any upstream request. Writes
// are never retried after a transport failure because the server may already
// have granted the reward or consumed a card.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type rewardAction string

const (
	rewardCheckin rewardAction = "checkin"
	rewardTravel  rewardAction = "travel"
	rewardTasks   rewardAction = "tasks"
	rewardMakeup  rewardAction = "makeup"
	rewardRedeem  rewardAction = "redeem"
	rewardLottery rewardAction = "lottery"
	rewardEnergy  rewardAction = "energy"
)

var rewardActionOrder = []rewardAction{
	rewardCheckin, rewardTravel, rewardTasks, rewardMakeup, rewardRedeem, rewardLottery, rewardEnergy,
}

var rewardActionLabels = map[rewardAction]string{
	rewardCheckin: "每日签到",
	rewardTravel:  "Buddy 旅行",
	rewardTasks:   "任务奖励",
	rewardMakeup:  "断登补签",
	rewardRedeem:  "连登兑换",
	rewardLottery: "开盲盒",
	rewardEnergy:  "Buddy 能量开盒",
}

func validRewardAction(v string) (rewardAction, bool) {
	a := rewardAction(strings.ToLower(strings.TrimSpace(v)))
	for _, known := range rewardActionOrder {
		if a == known {
			return a, true
		}
	}
	return "", false
}

type rewardStep struct {
	Action  string `json:"action"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type rewardResult struct {
	AuthIndex      string       `json:"auth_index"`
	AuthID         string       `json:"auth_id,omitempty"`
	Name           string       `json:"name"`
	Nickname       string       `json:"nickname,omitempty"`
	UID            string       `json:"uid,omitempty"`
	Actions        []string     `json:"actions"`
	Running        bool         `json:"running"`
	Success        bool         `json:"success"`
	NeedsAttention bool         `json:"needs_attention"`
	Credits        int64        `json:"credits_gained"`
	Steps          []rewardStep `json:"steps"`
	StartedAt      string       `json:"started_at,omitempty"`
	FinishedAt     string       `json:"finished_at,omitempty"`
}

type rewardAPIError struct {
	Status  int
	Message string
}

func (e *rewardAPIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

const workbuddyGrowthBase = "/v2/activity/growth"

// rewardCall uses the host transport/proxy policy and never retries a write.
func rewardCall(ctx context.Context, sa *storedAuth, method, path string, body any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, billingBaseFor(sa)+workbuddyGrowthBase+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	req.Header.Set("User-Agent", "WorkBuddy")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil || m == nil {
		return nil, &rewardAPIError{resp.StatusCode, "成长中心返回无效 JSON"}
	}
	code, hasCode := m["code"]
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || hasCode && code != float64(0) || rewardValue(m, "success") == false {
		msg := rewardString(m, "msg")
		if msg == "" {
			msg = rewardString(m, "message")
		}
		if msg == "" {
			msg = "成长中心请求失败"
		}
		return nil, &rewardAPIError{resp.StatusCode, truncateRedacted(msg, 180)}
	}
	return m, nil
}

func rewardValue(m map[string]any, key string) any {
	if v := m[key]; v != nil {
		return v
	}
	for _, wrapper := range []string{"data", "result", "resp", "response"} {
		if nested, ok := m[wrapper].(map[string]any); ok {
			if v := rewardValue(nested, key); v != nil {
				return v
			}
		}
	}
	return nil
}

func rewardString(m map[string]any, key string) string {
	s, _ := rewardValue(m, key).(string)
	return s
}

func rewardInt(m map[string]any, key string) int64 {
	return jsonI64(map[string]any{"v": rewardValue(m, key)}, "v")
}

func rewardToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "u" + hex.EncodeToString(b[:]), nil
}

type rewardRunner struct {
	ctx         context.Context
	sa          *storedAuth
	manual      bool
	result      rewardResult
	wrote       bool
	callForTest func(context.Context, string, string, any) (map[string]any, error)
}

func (r *rewardRunner) note(action, status, message string) {
	r.result.Steps = append(r.result.Steps, rewardStep{Action: action, Status: status, Message: message})
	if status == "error" {
		r.result.Success = false
		r.result.NeedsAttention = true
	}
}

func (r *rewardRunner) request(action, method, path string, body any) (map[string]any, bool) {
	if err := r.ctx.Err(); err != nil {
		r.note(action, "error", "本轮时间预算耗尽，剩余项下轮继续")
		return nil, false
	}
	var m map[string]any
	var err error
	if r.callForTest != nil {
		m, err = r.callForTest(r.ctx, method, path, body)
	} else {
		m, err = rewardCall(r.ctx, r.sa, method, path, body)
	}
	if err == nil {
		return m, true
	}
	var apiErr *rewardAPIError
	if errors.As(err, &apiErr) {
		msg := strings.ToLower(apiErr.Message)
		benign := path == "/makeup-cards/use" && apiErr.Status == 400 && msg == "date is not broken, no makeup needed" ||
			path == "/redeem" && apiErr.Status == 403 && strings.Contains(apiErr.Message, "不足") ||
			path == "/lottery/draw" && apiErr.Status == 400 && (strings.Contains(msg, "no chance") || (strings.Contains(msg, "insufficient") || strings.Contains(msg, "not enough")) && (strings.Contains(msg, "chance") || strings.Contains(msg, "balance")))
		if benign {
			r.note(action, "skipped", apiErr.Message)
			return nil, false
		}
	}
	r.note(action, "error", truncateRedacted(err.Error(), 200))
	return nil, false
}

func (r *rewardRunner) write(action, path string, body map[string]any, token bool) (map[string]any, bool) {
	if token {
		v, err := rewardToken()
		if err != nil {
			r.note(action, "error", "无法生成请求标识")
			return nil, false
		}
		body["client_token"] = v
	}
	m, ok := r.request(action, http.MethodPost, path, body)
	if ok {
		r.wrote = true
	}
	return m, ok
}

func runRewards(ctx context.Context, sa *storedAuth, actions []rewardAction, manual bool) (rewardResult, bool) {
	r := &rewardRunner{ctx: ctx, sa: sa, manual: manual, result: rewardResult{Success: true, Steps: []rewardStep{}}}
	// Keep the defense at the protocol boundary as well as the scheduler. A
	// future caller cannot invoke this runner directly with a Global auth.
	if serviceFor(sa) != serviceCN {
		r.result.Steps = append(r.result.Steps, rewardStep{Action: "奖励领取", Status: "skipped", Message: "国际版账号不参与签到与成长奖励"})
		return r.result, false
	}
	for _, action := range actions {
		if r.ctx.Err() != nil {
			r.note(string(action), "error", "本轮时间预算耗尽，剩余项下轮继续")
			break
		}
		switch action {
		case rewardCheckin:
			r.checkin()
		case rewardTravel:
			r.travel()
		case rewardTasks:
			r.tasks()
		case rewardMakeup:
			r.makeup()
		case rewardRedeem:
			r.redeem()
		case rewardLottery:
			r.lottery()
		case rewardEnergy:
			r.energy()
		}
	}
	return r.result, r.wrote
}

func (r *rewardRunner) checkin() {
	probe := checkinCapabilityProbe(r.sa, "")
	if probe.Summary != nil {
		probe.Summary.Capability = string(probe.Capability)
		probe.Summary.CapabilityNote = probe.Message
	}
	switch probe.Capability {
	case capabilityUnsupported:
		r.note(string(rewardCheckin), "skipped", probe.Message)
		return
	case capabilityUnknown:
		if !r.manual {
			r.note(string(rewardCheckin), "skipped", "签到状态未知，自动模式不发起领取")
			return
		}
	}
	if probe.Summary != nil && probe.Summary.TodayCheckedIn {
		r.note(string(rewardCheckin), "skipped", "今日已签到")
		return
	}
	res, err := performCheckinCall(r.sa)
	if err != nil {
		r.note(string(rewardCheckin), "error", truncateRedacted(err.Error(), 180))
		return
	}
	if res["success"] != true {
		msg, _ := res["message"].(string)
		if msg == "" {
			msg = "签到未成功"
		}
		r.note(string(rewardCheckin), "error", truncateRedacted(msg, 180))
		return
	}
	r.wrote = true
	credit := int64(0)
	for _, key := range []string{"credit", "reward_credit", "today_credit"} {
		if v := rewardInt(res, key); v > 0 {
			credit = v
			break
		}
	}
	r.result.Credits += credit
	msg := "签到成功"
	if credit > 0 {
		msg = fmt.Sprintf("签到成功 +%d 积分", credit)
	}
	r.note(string(rewardCheckin), "success", msg)
}

func (r *rewardRunner) travel() {
	m, ok := r.request(string(rewardTravel), http.MethodGet, "/buddy/travel/status", nil)
	if !ok {
		return
	}
	state := rewardString(m, "state")
	if state == "arrived" {
		id := rewardValue(m, "record_id")
		if id == nil {
			r.note(string(rewardTravel), "error", "缺少旅行记录 ID")
			return
		}
		reward, ok := r.write(string(rewardTravel), "/buddy/travel/claim", map[string]any{"record_id": id}, false)
		if !ok {
			return
		}
		if rewardValue(reward, "reward_credit") == nil {
			r.note(string(rewardTravel), "error", "响应未确认发放奖励，保留旅行状态")
			return
		}
		credits := rewardInt(reward, "reward_credit")
		r.result.Credits += credits
		r.note(string(rewardTravel), "success", fmt.Sprintf("领取旅行礼物 +%d 积分", credits))
		state = "idle"
	}
	if state == "traveling" {
		r.note(string(rewardTravel), "skipped", "Buddy 旅行中，下轮再查询")
		return
	}
	if state != "idle" {
		r.note(string(rewardTravel), "error", "未知旅行状态")
		return
	}
	if truthy(rewardValue(m, "daily_limit_reached")) {
		r.note(string(rewardTravel), "skipped", "今日旅行名额已用完")
		return
	}
	config, ok := r.request(string(rewardTravel), http.MethodGet, "/buddy/travel/config", nil)
	if !ok {
		return
	}
	locs, _ := rewardValue(config, "locations").([]any)
	if len(locs) == 0 {
		r.note(string(rewardTravel), "skipped", "暂无旅行地点")
		return
	}
	loc, _ := locs[0].(map[string]any)
	if loc["id"] == nil {
		r.note(string(rewardTravel), "error", "旅行地点缺少 ID")
		return
	}
	if _, ok := r.write(string(rewardTravel), "/buddy/travel/depart", map[string]any{"location_id": loc["id"]}, false); ok {
		r.note(string(rewardTravel), "success", "已派出 Buddy")
	}
}

func (r *rewardRunner) tasks() {
	m, ok := r.request(string(rewardTasks), http.MethodGet, "/tasks", nil)
	if !ok {
		return
	}
	tasks, ok := rewardValue(m, "tasks").([]any)
	if !ok {
		r.note(string(rewardTasks), "error", "任务列表格式无效")
		return
	}
	var pending []string
	for _, v := range tasks {
		t, ok := v.(map[string]any)
		if !ok {
			r.note(string(rewardTasks), "error", "任务记录格式无效")
			continue
		}
		code := rewardString(t, "task_code")
		if code != "" && !truthy(t["locked"]) && t["accept_status"] == "not_accepted" {
			pending = append(pending, code)
		}
	}
	for len(pending) > 0 {
		n := min(20, len(pending))
		batch := pending[:n]
		pending = pending[n:]
		m, ok := r.write(string(rewardTasks), "/tasks/accept", map[string]any{"task_codes": batch}, false)
		if !ok {
			continue
		}
		rows, ok := rewardValue(m, "results").([]any)
		if !ok {
			r.note(string(rewardTasks), "error", "接单结果缺失，请下轮查询状态")
			continue
		}
		for _, v := range rows {
			row, _ := v.(map[string]any)
			status := rewardString(row, "status")
			code := rewardString(row, "task_code")
			if status == "error" || status == "" {
				r.note(string(rewardTasks), "error", code+" "+truncateRedacted(rewardString(row, "message"), 160))
			} else {
				r.note(string(rewardTasks), "success", "已领取任务 "+code)
			}
		}
	}
	for _, v := range tasks {
		t, _ := v.(map[string]any)
		code := rewardString(t, "task_code")
		if code == "" || truthy(t["locked"]) || t["accept_status"] != "completed" {
			continue
		}
		m, ok := r.write(string(rewardTasks), "/tasks/"+url.PathEscape(code)+"/claim", map[string]any{}, false)
		if !ok {
			continue
		}
		if truthy(rewardValue(m, "already_claimed")) {
			r.note(string(rewardTasks), "skipped", "任务奖 "+code+" 已领取")
			continue
		}
		credits := rewardInt(m, "credit")
		if rewardValue(m, "credit") == nil {
			credits = rewardInt(t, "reward_credit")
		}
		r.result.Credits += credits
		r.note(string(rewardTasks), "success", fmt.Sprintf("领取任务奖 %s +%d 积分", code, credits))
	}
}

// rewardMakeupDate only accepts explicit zero-score calendar cells. Missing,
// invalid, or conflicting calendar data returns an error instead of spending a
// card on a guessed date.
func rewardMakeupDate(streak, heatmap map[string]any) (string, error) {
	parse := func(v any) (time.Time, error) {
		s, ok := v.(string)
		if !ok {
			return time.Time{}, errors.New("补登日期缺失")
		}
		return time.Parse("2006-01-02", s)
	}
	todayObj, _ := rewardValue(heatmap, "today").(map[string]any)
	today, err := parse(todayObj["date"])
	if err != nil {
		return "", errors.New("活跃日历缺少服务端日期")
	}
	launch, err := parse(rewardValue(streak, "launch_date"))
	if err != nil {
		return "", errors.New("连登上线日期缺失")
	}
	start := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	if launch.After(start) {
		start = launch
	}
	floor := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	if floor.After(start) {
		start = floor
	}
	st, ok := rewardValue(streak, "streak").(map[string]any)
	if !ok {
		return "", errors.New("连登状态格式无效")
	}
	history := map[string]bool{}
	if v, exists := st["makeup_dates"]; exists {
		rows, ok := v.([]any)
		if !ok {
			return "", errors.New("已补登记录格式无效")
		}
		for _, raw := range rows {
			d, err := parse(raw)
			if err != nil {
				return "", err
			}
			history[d.Format("2006-01-02")] = true
		}
	}
	cells, ok := rewardValue(heatmap, "cells").([]any)
	if !ok {
		return "", errors.New("活跃日历缺少日期列表")
	}
	scores := map[string]float64{}
	for _, v := range cells {
		cell, ok := v.(map[string]any)
		if !ok {
			return "", errors.New("活跃日历记录格式无效")
		}
		d, err := parse(cell["date"])
		if err != nil {
			return "", err
		}
		score, ok := cell["score"].(float64)
		if !ok || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 {
			return "", errors.New("活跃日历分数无效")
		}
		key := d.Format("2006-01-02")
		if prev, exists := scores[key]; exists && prev != score {
			return "", errors.New("活跃日历同一日期记录冲突")
		}
		scores[key] = score
	}
	var dates []string
	for d, score := range scores {
		day, _ := parse(d)
		if !day.Before(start) && day.Before(today) && score == 0 && !history[d] {
			dates = append(dates, d)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	if len(dates) > 0 {
		return dates[0], nil
	}
	return "", nil
}

func (r *rewardRunner) makeup() {
	m, ok := r.request(string(rewardMakeup), http.MethodGet, "/streak", nil)
	if !ok {
		return
	}
	cards := rewardValue(m, "makeup_cards")
	if obj, ok := cards.(map[string]any); ok {
		cards = obj["balance"]
	}
	if jsonI64(map[string]any{"cards": cards}, "cards") <= 0 {
		r.note(string(rewardMakeup), "skipped", "暂无补登卡")
		return
	}
	calendar, ok := r.request(string(rewardMakeup), http.MethodGet, "/heatmap", nil)
	if !ok {
		return
	}
	date, err := rewardMakeupDate(m, calendar)
	if err != nil {
		r.note(string(rewardMakeup), "error", err.Error())
		return
	}
	if date == "" {
		r.note(string(rewardMakeup), "skipped", "没有可补登的断签日期")
		return
	}
	if _, ok := r.write(string(rewardMakeup), "/makeup-cards/use", map[string]any{"target_date": date}, false); ok {
		r.note(string(rewardMakeup), "success", date+" 已补登（本轮一张卡）")
	}
}

func (r *rewardRunner) redeem() {
	m, ok := r.request(string(rewardRedeem), http.MethodGet, "/redeem/summary", nil)
	if !ok {
		return
	}
	for _, tier := range []struct {
		key, id string
		days    int
	}{{"starter", "7d", 7}, {"advanced", "14d", 14}, {"legendary", "28d", 28}} {
		status := rewardString(m, tier.key+"_status")
		if status == "" || status == "locked" || status == "claimed" {
			continue
		}
		result, ok := r.write(string(rewardRedeem), "/redeem", map[string]any{"tier": tier.id}, true)
		if ok {
			credit := rewardInt(result, "credit_granted")
			r.result.Credits += credit
			r.note(string(rewardRedeem), "success", fmt.Sprintf("连登兑换 %s 成功 +%d 积分", tier.id, credit))
		}
	}
}

func (r *rewardRunner) lottery() {
	m, ok := r.request(string(rewardLottery), http.MethodGet, "/lottery/chances", nil)
	if !ok || rewardInt(m, "balance") <= 0 {
		return
	}
	prize, ok := r.write(string(rewardLottery), "/lottery/draw", map[string]any{}, true)
	if !ok {
		return
	}
	name := rewardString(prize, "prize_name")
	if name == "" {
		name = rewardString(prize, "prize")
	}
	if name == "" {
		name = "已获得奖品，请到成长中心查看"
	}
	if truthy(rewardValue(prize, "need_address")) || truthy(rewardValue(prize, "require_address")) {
		name += "（实物奖：请到成长中心填写收件信息）"
		r.result.NeedsAttention = true
	}
	r.note(string(rewardLottery), "success", name)
}

func (r *rewardRunner) energy() {
	m, ok := r.request(string(rewardEnergy), http.MethodGet, "/buddy/quota", nil)
	if !ok || rewardInt(m, "affordable") <= 0 {
		return
	}
	count := min(rewardInt(m, "affordable"), max(int64(1), rewardInt(m, "max_open_count")))
	if _, ok := r.write(string(rewardEnergy), "/buddy/open", map[string]any{"count": count}, true); ok {
		r.note(string(rewardEnergy), "success", fmt.Sprintf("已开 %d 个 Buddy 盲盒", count))
	}
}
