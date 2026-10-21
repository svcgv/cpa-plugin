// Qoder CN daily Credits campaigns. Protocol adapted from
// 88lin/workbuddy-auto-signin; see ../THIRD_PARTY_NOTICES.md.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type rewardAction string

const rewardCheckin rewardAction = "checkin"

var rewardActionOrder = []rewardAction{rewardCheckin}

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

func validRewardAction(value string) (rewardAction, bool) {
	if strings.EqualFold(strings.TrimSpace(value), string(rewardCheckin)) {
		return rewardCheckin, true
	}
	return "", false
}

func qoderRewardKey(sa *storedAuth, authID string) string {
	region := "intl"
	uid, enterpriseID := "", ""
	if sa != nil {
		if sa.isCN() {
			region = "cn"
		}
		uid = sa.Account.UID
		enterpriseID = sa.Account.EnterpriseID
	}
	return rewardAccountKey("qoderwork", region, uid, enterpriseID, authID)
}

// rewardAccountKey is intentionally duplicated from WorkBuddy (the plugins are
// separate Go modules). Only a hash is persisted as an account override key.
func rewardAccountKey(provider, region, uid, enterpriseID, authID string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = "cn"
	}
	uid, enterpriseID, authID = strings.TrimSpace(uid), strings.TrimSpace(enterpriseID), strings.TrimSpace(authID)
	if uid == "" {
		uid = "auth:" + authID
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{provider, region, uid, enterpriseID}, "\x00")))
	return region + "-" + hex.EncodeToString(sum[:])[:16]
}

type rewardSettings struct {
	Auto    bool
	Actions map[rewardAction]bool
}

type rewardOverride struct {
	Auto    *bool
	Actions map[rewardAction]*bool
}

type rewardConfig struct {
	Defaults rewardSettings
	Accounts map[string]rewardOverride
}

var rewardConfigState = struct {
	sync.RWMutex
	cfg rewardConfig
}{
	cfg: rewardConfig{Defaults: rewardSettings{Auto: true, Actions: map[rewardAction]bool{rewardCheckin: true}}, Accounts: map[string]rewardOverride{}},
}

func rewardSettingsFor(sa *storedAuth, authID string) rewardSettings {
	rewardConfigState.RLock()
	defer rewardConfigState.RUnlock()
	settings := rewardSettings{Auto: rewardConfigState.cfg.Defaults.Auto, Actions: map[rewardAction]bool{}}
	for action, enabled := range rewardConfigState.cfg.Defaults.Actions {
		settings.Actions[action] = enabled
	}
	if override, ok := rewardConfigState.cfg.Accounts[qoderRewardKey(sa, authID)]; ok {
		if override.Auto != nil {
			settings.Auto = *override.Auto
		}
		for action, enabled := range override.Actions {
			if enabled != nil {
				settings.Actions[action] = *enabled
			}
		}
	}
	return settings
}

func setRewardConfig(cfg rewardConfig) {
	rewardConfigState.Lock()
	rewardConfigState.cfg = cfg
	rewardConfigState.Unlock()
}

func rewardConfigSnapshot() rewardSettings {
	rewardConfigState.RLock()
	defer rewardConfigState.RUnlock()
	out := rewardSettings{Auto: rewardConfigState.cfg.Defaults.Auto, Actions: map[rewardAction]bool{}}
	for action, enabled := range rewardConfigState.cfg.Defaults.Actions {
		out.Actions[action] = enabled
	}
	return out
}

type rewardCampaign struct {
	ID          string `json:"campaignId"`
	Key         string `json:"campaignKey"`
	Action      string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     struct {
		Amount float64 `json:"amount"`
	} `json:"benefit"`
}

func (c rewardCampaign) active(now time.Time) bool {
	return c.Action == "CLAIM_BENEFIT" &&
		(c.StartAt == 0 || now.Unix() >= c.StartAt) &&
		(c.EndAt == 0 || now.Unix() < c.EndAt)
}

func (c rewardCampaign) displayKey() string {
	if strings.TrimSpace(c.Key) != "" {
		return c.Key
	}
	return c.ID
}

func campaignHeaders(req *http.Request, sa *storedAuth) {
	billingHeaders(req, sa)
	if sa != nil && sa.Auth.MachineID != "" {
		req.Header.Set("Cosy-MachineId", sa.Auth.MachineID)
	}
	official := readOfficialMachineIdentity()
	if sa != nil && official.MachineID != "" && official.MachineID == sa.Auth.MachineID {
		for key, value := range map[string]string{
			"Cosy-MachineToken": official.MachineToken,
			"Cosy-MachineType":  official.MachineType,
			"Cosy-MachineOS":    official.MachineOS,
			"Cosy-Version":      official.CosyVersion,
		} {
			if value != "" {
				req.Header.Set(key, value)
			}
		}
	}
}

func campaignRequest(ctx context.Context, sa *storedAuth, path, method string) (*hostHTTPResponse, error) {
	if sa == nil || !sa.isCN() {
		return nil, errCheckinUnsupported
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, upstreamBaseCN+"/sash/api/v1/me/campaigns"+path, nil)
	if err != nil {
		return nil, err
	}
	campaignHeaders(req, sa)
	return hostHTTPDo(req)
}

var errCampaignsUnavailable = errors.New("campaign endpoint unavailable")

func fetchCampaigns(ctx context.Context, sa *storedAuth) ([]rewardCampaign, error) {
	resp, err := campaignRequest(ctx, sa, "", http.MethodGet)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, errCampaignsUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("活动查询 HTTP %d: %s", resp.StatusCode, truncateRedacted(string(resp.Body), 180))
	}
	var envelope struct {
		Campaigns json.RawMessage `json:"campaigns"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return nil, fmt.Errorf("活动返回解析失败: %w", err)
	}
	if len(envelope.Campaigns) == 0 || string(envelope.Campaigns) == "null" {
		return nil, errors.New("活动列表缺失")
	}
	var rows []rewardCampaign
	if err := json.Unmarshal(envelope.Campaigns, &rows); err != nil {
		return nil, fmt.Errorf("活动列表格式无效: %w", err)
	}
	return rows, nil
}

func campaignSummary(rows []rewardCampaign, now time.Time) *checkinSummary {
	summary := &checkinSummary{ActivityName: "每日 Credits 活动（10:00 UTC+8 刷新）"}
	var claimable, claimed int
	for _, campaign := range rows {
		if !campaign.active(now) {
			continue
		}
		summary.Active = true
		switch campaign.ClaimStatus {
		case "CLAIMABLE":
			claimable++
			summary.DailyCredit += int64(campaign.Benefit.Amount)
		case "CLAIMED":
			claimed++
			summary.TodayCredit += int64(campaign.Benefit.Amount)
		}
	}
	summary.TodayCheckedIn = claimed > 0 && claimable == 0
	return summary
}

// claimCampaigns performs one pass over currently claimable campaigns. No write
// is retried. Claim success requires status=CLAIMED in the response.
func claimCampaigns(ctx context.Context, sa *storedAuth, rows []rewardCampaign) (map[string]any, error) {
	out := map[string]any{"success": true}
	var credits float64
	var claimed []string
	var failures []string
	already := false
	seen := map[string]bool{}
	for _, campaign := range rows {
		if !campaign.active(time.Now()) {
			continue
		}
		if campaign.ClaimStatus == "CLAIMED" {
			already = true
			continue
		}
		if campaign.ClaimStatus != "CLAIMABLE" {
			continue
		}
		if campaign.ID == "" {
			failures = append(failures, "活动缺少 ID")
			continue
		}
		if seen[campaign.ID] {
			continue
		}
		seen[campaign.ID] = true
		if err := ctx.Err(); err != nil {
			failures = append(failures, "本轮时间预算耗尽")
			break
		}
		resp, err := campaignRequest(ctx, sa, "/"+url.PathEscape(campaign.ID)+"/claim", http.MethodPost)
		if err != nil {
			failures = append(failures, campaign.displayKey()+": "+truncateRedacted(err.Error(), 160))
			continue
		}
		var envelope map[string]json.RawMessage
		_ = json.Unmarshal(resp.Body, &envelope)
		body := resp.Body
		if data, ok := envelope["data"]; ok {
			body = data
		}
		var claim struct {
			Status   string `json:"status"`
			Replayed bool   `json:"replayed"`
			Benefit  struct {
				Amount *float64 `json:"amount"`
			} `json:"benefit"`
		}
		decodeErr := json.Unmarshal(body, &claim)
		if resp.StatusCode != http.StatusOK || decodeErr != nil || claim.Status != "CLAIMED" {
			failures = append(failures, fmt.Sprintf("%s: HTTP %d %s", campaign.displayKey(), resp.StatusCode, truncateRedacted(string(resp.Body), 160)))
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				break
			}
			continue
		}
		if claim.Replayed {
			already = true
			continue
		}
		amount := campaign.Benefit.Amount
		if claim.Benefit.Amount != nil {
			amount = *claim.Benefit.Amount
		}
		credits += amount
		claimed = append(claimed, campaign.displayKey())
	}
	out["rewardCredits"] = credits
	out["campaigns"] = claimed
	switch {
	case len(failures) > 0:
		out["success"] = false
		out["needs_attention"] = true
		out["message"] = strings.Join(failures, "；")
	case len(claimed) > 0:
		out["message"] = fmt.Sprintf("成功领取 %g Credits", credits)
	case already:
		out["result"] = "ALREADY_CLAIMED"
		out["skipped"] = true
		out["reason"] = "already"
		out["message"] = "本活动窗口已领取"
	default:
		out["skipped"] = true
		out["reason"] = "inactive"
		out["message"] = "当前没有可领取活动"
	}
	return out, nil
}

func sortCampaigns(rows []rewardCampaign) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].StartAt != rows[j].StartAt {
			return rows[i].StartAt < rows[j].StartAt
		}
		return rows[i].displayKey() < rows[j].displayKey()
	})
}

var campaignChinaZone = time.FixedZone("Asia/Shanghai", 8*60*60)
var campaignHours = []int{10, 11, 15, 19, 23}

// fetchCheckinStatus follows the current campaign API and falls back to the
// legacy daily check-in endpoint only when the campaign route is genuinely
// absent (404/405).
func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := fetchCampaigns(ctx, sa)
	if errors.Is(err, errCampaignsUnavailable) {
		return fetchLegacyCheckinStatus(sa)
	}
	if err != nil {
		return nil, err
	}
	sortCampaigns(rows)
	return campaignSummary(rows, time.Now()), nil
}

func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	if sa == nil || !sa.isCN() {
		return nil, errCheckinUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	rows, err := fetchCampaigns(ctx, sa)
	if errors.Is(err, errCampaignsUnavailable) {
		return performLegacyCheckinCall(sa)
	}
	if err != nil {
		return nil, err
	}
	sortCampaigns(rows)
	return claimCampaigns(ctx, sa, rows)
}
