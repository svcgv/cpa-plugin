// checkin_capability.go decides whether an account can check in daily, and
// probes the two upstream activities that expose it.
//
// Not every CodeBuddy/WorkBuddy account has a daily check-in activity. The CN
// service exposes the "Buddy 加油站" activity; the international services have
// only been observed with a one-shot trial pack. A panel that offers 签到 to an
// account without an activity produces a confusing failure, so the capability is
// probed and reported instead of assumed.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// checkinCapability is the panel-facing verdict for one account.
type checkinCapability string

const (
	// capabilitySupported means an active daily check-in activity was observed.
	capabilitySupported checkinCapability = "supported"
	// capabilityUnsupported means every candidate endpoint answered, and none
	// exposes a check-in activity for this account.
	capabilityUnsupported checkinCapability = "unsupported"
	// capabilityUnknown means the probe could not reach a verdict (transport,
	// auth, rate limit, malformed body). Unknown never blocks a manual attempt
	// and never authorizes an automatic one.
	capabilityUnknown checkinCapability = "unknown"
)

// checkinProbe is the outcome of one capability probe.
type checkinProbe struct {
	Capability checkinCapability
	Summary    *checkinSummary
	// Message explains an unsupported or unknown verdict to the panel user.
	Message string
}

// checkinEndpointUnavailable reports whether an error means the endpoint itself
// is absent for this account, as opposed to a transient or auth failure.
// Only a real 404/405 (or the "not found"-style business codes) justifies
// falling through to the legacy status endpoint.
func checkinEndpointUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.HasPrefix(msg, "http 404") || strings.HasPrefix(msg, "http 405") {
		return true
	}
	if strings.HasPrefix(msg, "http 4") {
		// Other 4xx on the activity endpoint are auth/limit problems, not
		// evidence that the endpoint is missing.
		return false
	}
	return false
}

// checkinCapabilityProbe probes the account's service for a daily check-in
// activity. It tries the current activity endpoint first and the legacy status
// endpoint only when the first one is absent.
func checkinCapabilityProbe(sa *storedAuth, callbackID string) checkinProbe {
	paths := []string{
		"/v2/billing/meter/checkin-activity-status",
		"/v2/billing/meter/checkin-status",
	}
	sawDefinitiveMiss := false
	for _, path := range paths {
		data, err := billingCallWithCallback(sa, path, nil, callbackID)
		if err != nil {
			if checkinEndpointUnavailable(err) {
				sawDefinitiveMiss = true
				continue
			}
			// Transport, auth or limit failure: we cannot claim the account is
			// unsupported.
			return checkinProbe{
				Capability: capabilityUnknown,
				Message:    "签到状态查询失败：" + truncateRedacted(err.Error(), 120),
			}
		}
		sum, perr := parseCheckinSummary(data)
		if perr != nil {
			return checkinProbe{
				Capability: capabilityUnknown,
				Message:    "签到状态返回无法解析，请稍后重试",
			}
		}
		if !sum.Active {
			// The endpoint answered but reports no running activity. Treat it
			// as unsupported only when the payload actually carried an activity
			// flag; an empty payload is inconclusive.
			if sum.actedOnActivity() {
				return checkinProbe{
					Capability: capabilityUnsupported,
					Summary:    sum,
					Message:    "该账号当前没有可参与的每日签到活动",
				}
			}
			return checkinProbe{
				Capability: capabilityUnknown,
				Summary:    sum,
				Message:    "签到活动状态未返回，无法确认",
			}
		}
		return checkinProbe{Capability: capabilitySupported, Summary: sum}
	}
	if sawDefinitiveMiss {
		return checkinProbe{
			Capability: capabilityUnsupported,
			Message:    "当前服务未提供每日签到接口",
		}
	}
	return checkinProbe{Capability: capabilityUnknown, Message: "签到状态未知"}
}

// parseCheckinSummary decodes a check-in status payload.
func parseCheckinSummary(data json.RawMessage) (*checkinSummary, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	sum := &checkinSummary{
		TodayCheckedIn:  jsonBool(m, "today_checked_in", "todayCheckedIn"),
		StreakDays:      jsonI64(m, "streak_days", "streakDays"),
		DailyCredit:     jsonI64(m, "daily_credit", "dailyCredit"),
		TodayCredit:     jsonI64(m, "today_credit", "todayCredit"),
		TotalCredits:    jsonI64(m, "total_credits", "totalCredits"),
		WeekCheckinDays: jsonI64(m, "week_checkin_days", "weekCheckinDays"),
		ActivityName:    jsonStr(m, "activity_name", "activityName"),
		Season:          jsonI64(m, "season", "season"),
	}
	// `active` is tri-state: present-and-true, present-and-false, or absent.
	// Callers must not read an absent flag as "no activity".
	if v, ok := lookupAny(m, "active", "Active"); ok {
		sum.Active = truthy(v)
		sum.activityFlagPresent = true
	}
	if dates, ok := m["checkin_dates"].([]any); ok {
		for _, d := range dates {
			if s, ok := d.(string); ok {
				sum.CheckinDates = append(sum.CheckinDates, s)
			}
		}
	} else if dates, ok := m["checkinDates"].([]any); ok {
		for _, d := range dates {
			if s, ok := d.(string); ok {
				sum.CheckinDates = append(sum.CheckinDates, s)
			}
		}
	}
	return sum, nil
}

func lookupAny(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return nil, false
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int64:
		return t != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1"
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n != 0
		}
		if f, err := strconv.ParseFloat(t.String(), 64); err == nil {
			return f != 0
		}
	}
	return false
}
