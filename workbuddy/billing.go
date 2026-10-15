// billing.go owns the upstream billing API surface: check-in status, user
// resource (credits / packages), payment type, and the perform-* call wrappers
// for daily check-in and trial claim. Includes the shared JSON helpers used
// to tolerate the upstream's loosely-typed response shapes, and the region
// helpers that decide CN vs Global endpoint.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const userResourceConcurrency = 4

var userResourceSlots = make(chan struct{}, userResourceConcurrency)

func acquireUserResourceSlot() func() {
	userResourceSlots <- struct{}{}
	return func() { <-userResourceSlots }
}

// -----------------------------------------------------------------------------
// Billing / check-in API calls
// -----------------------------------------------------------------------------

func billingHeaders(req *http.Request, sa *storedAuth) {
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", sa.Account.EnterpriseID)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	}
}

func billingCall(sa *storedAuth, path string, body any) (json.RawMessage, error) {
	return billingCallWithCallback(sa, path, body, "")
}

func billingCallWithCallback(sa *storedAuth, path string, body any, callbackID string) (json.RawMessage, error) {
	data, err := billingCallOnceWithCallback(sa, path, body, callbackID)
	for _, d := range billingRetryDelays {
		if err == nil || !isTransientBillingErr(err) {
			break
		}
		time.Sleep(d)
		data, err = billingCallOnceWithCallback(sa, path, body, callbackID)
	}
	return data, err
}

// isTransientBillingErr reports whether err came from an upstream 5xx or a
// transport failure (both retryable). 4xx and business-code errors are not.
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "http 5") || strings.HasPrefix(msg, "http=5") || strings.Contains(msg, "status 5")
}

func billingCallOnce(sa *storedAuth, path string, body any) (json.RawMessage, error) {
	return billingCallOnceWithCallback(sa, path, body, "")
}

func billingCallOnceWithCallback(sa *storedAuth, path string, body any, callbackID string) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader([]byte("{}"))
	}
	base := billingBaseFor(sa)
	req, err := http.NewRequest(http.MethodPost, base+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	// Route via host.http.do so request-log captures the call (v0.8.1 compliance:
	// was sharedHTTPClient().Do — bypassed host transport policy + logging).
	resp, err := hostHTTPDoWithCallback(req, callbackID)
	if err != nil {
		return nil, err
	}
	raw := resp.Body
	// Upstream 5xx is transient — classify it so billingCall can retry,
	// and keep a redacted response body snippet for diagnosis (A-42).
	if resp.StatusCode >= 500 {
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, snippet)
	}
	// A 4xx body is often HTML or a bare status page rather than the usual
	// {code,msg,data} envelope. Surface the HTTP status in that case so callers
	// can distinguish "this endpoint does not exist" from "the response body is
	// unreadable" — the callers that probe for optional endpoints depend on it.
	if resp.StatusCode >= 400 {
		var errEnv apiEnvelope
		if err := json.Unmarshal(raw, &errEnv); err == nil && errEnv.Code != 0 {
			return nil, fmt.Errorf("code=%d msg=%s", errEnv.Code, truncateRedacted(errEnv.Msg, 120))
		}
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, snippet)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// parse failed usually means upstream returned a non-JSON error page
		// (e.g. APISIX 401 HTML for session-dead). Include a redacted snippet
		// so the panel / logs can surface the real cause instead of a bare
		// "parse failed" (P0-2 UX: was impossible to distinguish session dead
		// from a malformed response).
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, snippet)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, nil
}

func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	return fetchCheckinStatusWithCallback(sa, "")
}

// fetchCheckinStatusWithCallback probes the account's check-in activity. Shared
// with the capability probe so the panel and the scheduler always agree on
// whether an account can check in.
func fetchCheckinStatusWithCallback(sa *storedAuth, callbackID string) (*checkinSummary, error) {
	probe := checkinCapabilityProbe(sa, callbackID)
	if probe.Summary != nil {
		probe.Summary.Capability = string(probe.Capability)
		probe.Summary.CapabilityNote = probe.Message
		return probe.Summary, nil
	}
	if probe.Capability == capabilityUnknown {
		if probe.Message == "" {
			probe.Message = "签到状态未知"
		}
		return nil, fmt.Errorf("%s", probe.Message)
	}
	// Supported or unsupported, but no parsed summary: hand back the verdict so
	// the caller can still render a capability badge.
	return &checkinSummary{
		Capability:     string(probe.Capability),
		CapabilityNote: probe.Message,
	}, nil
}

// packageRemainUsed picks current-cycle remain/used/size for one package.
// Prefer cycle metrics whenever CycleCapacitySize is present; used = size−remain
// so missing CycleCapacityUsed never under-reports consumption.
// Fall back to lifetime Capacity* only when cycle fields are absent entirely.
//
// Daily check-in adds NEW packages (size grows) — capacity grant, not negative
// consumption. Track consumption via used (size−remain), not via remain alone.
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		// If upstream reports a higher explicit used, trust the larger figure.
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			// Keep remain consistent when possible.
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	if a.CycleCapacityRemain > 0 || a.CycleCapacityUsed > 0 {
		remain = a.CycleCapacityRemain
		used = a.CycleCapacityUsed
		// A-41: clamp negatives (branch1 already clamps; branch2/3 did not).
		if remain < 0 {
			remain = 0
		}
		if used < 0 {
			used = 0
		}
		size = remain + used
		if a.CapacitySize > size {
			size = a.CapacitySize
			if size >= remain {
				used = size - remain
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	// A-41: lifetime branch also clamps negative remain/used.
	if remain < 0 {
		remain = 0
	}
	if used < 0 {
		used = 0
	}
	if size <= 0 {
		size = remain + used
	}
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

var errEnterpriseCreditsUnsupported = errors.New("enterprise credits unsupported")

func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	return fetchUserResourceWithCallback(sa, "")
}

func fetchUserResourceWithCallback(sa *storedAuth, callbackID string) (*creditsSummary, error) {
	release := acquireUserResourceSlot()
	defer release()

	cfg := currentFeatureRuntime()
	if cfg != nil && cfg.enterpriseCredits && accountRegion(sa) == "cn" {
		credits, err := fetchEnterpriseCreditsCN(sa, callbackID)
		if err == nil {
			return credits, nil
		}
		if !errors.Is(err, errEnterpriseCreditsUnsupported) {
			return nil, err
		}
	}
	return fetchPersonalUserResourceWithCallback(sa, callbackID)
}

func fetchPersonalUserResourceWithCallback(sa *storedAuth, callbackID string) (*creditsSummary, error) {
	now := time.Now()
	// Status 0=active, 3=exhausted-but-still-listed.
	const pageSize = 100
	baseBody := map[string]any{
		"PageSize":                 pageSize,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	var all []resourcePackage
	var totalCount int64
	var totalDosage int64
	for page := 1; ; page++ {
		body := make(map[string]any, len(baseBody)+1)
		for k, v := range baseBody {
			body[k] = v
		}
		body["PageNumber"] = page
		data, err := billingCallWithCallback(sa, "/v2/billing/meter/get-user-resource", body, callbackID)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Response struct {
				Data struct {
					TotalCount  int64             `json:"TotalCount"`
					TotalDosage int64             `json:"TotalDosage"` // package capacity pool, NOT consumption
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, err
		}
		if page == 1 {
			totalCount = resp.Response.Data.TotalCount
			totalDosage = resp.Response.Data.TotalDosage
		}
		all = append(all, resp.Response.Data.Accounts...)
		if len(resp.Response.Data.Accounts) < pageSize || (totalCount > 0 && totalCount <= int64(len(all))) {
			break
		}
	}
	// Aggregate ALL packages (体验版 + 多个签到/裂变包 + 其它赠送包).
	// Remain = currently spendable. Used = consumed this cycle. Size = capacity.
	// Daily check-in adds packages → Size and Remain go UP; that is grant, not usage.
	sum := &creditsSummary{}
	for _, a := range all {
		remain, used, size := packageRemainUsed(a)
		sum.TotalRemain += remain
		sum.TotalUsed += used
		sum.TotalSize += size
		sum.Packages = append(sum.Packages, packageSummary{
			Name:       a.PackageName,
			Remain:     remain,
			Used:       used,
			Size:       size,
			CycleStart: a.CycleStartTime,
			CycleEnd:   a.CycleEndTime,
		})
	}
	sum.PackCount = len(sum.Packages)
	// Reconcile used with size-remain so UI totals always add up when size known.
	if sum.TotalSize > 0 {
		derived := sum.TotalSize - sum.TotalRemain
		if derived < 0 {
			derived = 0
		}
		// Prefer the larger of reported-used vs size-remain (never under-report spend).
		if derived > sum.TotalUsed {
			sum.TotalUsed = derived
		}
	}
	// Upstream TotalDosage is the capacity pool (~sum of package sizes), not spend.
	// Use it only as a size floor when pack sizes look incomplete.
	if totalDosage > sum.TotalSize {
		sum.TotalSize = totalDosage
		derived := sum.TotalSize - sum.TotalRemain
		if derived < 0 {
			derived = 0
		}
		if derived > sum.TotalUsed {
			sum.TotalUsed = derived
		}
	}
	return sum, nil
}

func fetchEnterpriseCreditsCN(sa *storedAuth, callbackID string) (*creditsSummary, error) {
	req, err := http.NewRequest(http.MethodPost, billingBaseFor(sa)+"/billing/meter/get-enterprise-user-usage", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Client-Platform", "web")
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("Referer", "https://www.codebuddy.cn/")

	resp, err := hostHTTPDoWithCallback(req, callbackID)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, classifyEnterpriseHTTPStatus(resp.StatusCode)
	}
	return parseEnterpriseCredits(resp.Body)
}

func classifyEnterpriseHTTPStatus(status int) error {
	if status == http.StatusNotFound {
		return fmt.Errorf("enterprise credits status %d: %w", status, errEnterpriseCreditsUnsupported)
	}
	return fmt.Errorf("enterprise credits status %d", status)
}

func parseEnterpriseCredits(raw []byte) (*creditsSummary, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parse enterprise credits: %w", err)
	}
	codeRaw, ok := envelope["code"]
	if !ok {
		return nil, errors.New("enterprise credits response missing code")
	}
	var code *int
	if err := json.Unmarshal(codeRaw, &code); err != nil || code == nil || *code != 0 {
		return nil, errors.New("enterprise credits response rejected")
	}
	dataRaw, ok := envelope["data"]
	if !ok {
		return nil, errors.New("enterprise credits response missing data")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(dataRaw, &data); err != nil || data == nil {
		return nil, errors.New("enterprise credits response has invalid data")
	}
	credit, err := enterpriseCreditNumber(data, "credit")
	if err != nil {
		return nil, err
	}
	limit, err := enterpriseCreditNumber(data, "limitNum")
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("enterprise credits limit must be positive")
	}
	used, err := roundedEnterpriseCredits(credit)
	if err != nil {
		return nil, err
	}
	size, err := roundedEnterpriseCredits(limit)
	if err != nil {
		return nil, err
	}
	remain := size - used
	if remain < 0 {
		remain = 0
	}
	return &creditsSummary{
		TotalRemain: remain,
		TotalUsed:   used,
		TotalSize:   size,
		PackCount:   1,
		Packages: []packageSummary{{
			Name:       "Enterprise",
			Remain:     remain,
			Used:       used,
			Size:       size,
			CycleStart: enterpriseCreditString(data["cycleStartTime"]),
			CycleEnd:   enterpriseCreditString(data["cycleEndTime"]),
		}},
	}, nil
}

func enterpriseCreditNumber(data map[string]json.RawMessage, key string) (float64, error) {
	raw, ok := data[key]
	if !ok {
		return 0, fmt.Errorf("enterprise credits response missing %s", key)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0, fmt.Errorf("enterprise credits response has invalid %s", key)
	}
	var text string
	switch v := value.(type) {
	case json.Number:
		text = v.String()
	case string:
		text = v
	default:
		return 0, fmt.Errorf("enterprise credits response has invalid %s", key)
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, fmt.Errorf("enterprise credits response has invalid %s", key)
	}
	return number, nil
}

func roundedEnterpriseCredits(value float64) (int64, error) {
	rounded := math.Round(value)
	if rounded >= 1<<63 {
		return 0, errors.New("enterprise credits value is too large")
	}
	return int64(rounded), nil
}

func enterpriseCreditString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func fetchPaymentType(sa *storedAuth) string {
	return fetchPaymentTypeWithCallback(sa, "")
}

func fetchPaymentTypeWithCallback(sa *storedAuth, callbackID string) string {
	data, err := billingCallWithCallback(sa, "/v2/billing/meter/get-payment-type", nil, callbackID)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	if s, ok := m["paymentType"].(string); ok {
		return s
	}
	return ""
}

func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	return performCheckinCallWithCallback(sa, "")
}

func performCheckinCallWithCallback(sa *storedAuth, callbackID string) (map[string]any, error) {
	data, err := billingCallWithCallback(sa, "/v2/billing/meter/daily-checkin", nil, callbackID)
	if err != nil {
		// billingCall returns business errors (code != 0) as Go errors; surface
		// them as a structured result so the panel can show "already checked in".
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	// Normalize `success` to a real bool, but never overwrite an explicit
	// upstream false: a business-level rejection must not be reported as a
	// successful check-in. Upstream may send "true"/"false" as strings, and a
	// missing field means the call itself succeeded.
	if v, ok := m["success"]; ok {
		m["success"] = truthy(v)
	} else {
		m["success"] = true
	}
	return m, nil
}

// performTrialCall claims the one-time expert trial pack for a Global account.
// Endpoint: POST /billing/ide/trial (note: NOT under /v2/billing/meter/).
// First call: success, +250 credits, 14-day "CodeBuddy One-time Free 2-Week
// Pro Plan Trial".
// Repeat call: code=14051 "has applied trial" — surfaced as already_claimed.
func performTrialCall(sa *storedAuth) (map[string]any, error) {
	return performTrialCallWithCallback(sa, "")
}

func performTrialCallWithCallback(sa *storedAuth, callbackID string) (map[string]any, error) {
	data, err := billingCallWithCallback(sa, "/billing/ide/trial", nil, callbackID)
	if err != nil {
		msg := err.Error()
		// code=14051 means the trial has already been claimed — not a real error.
		if strings.Contains(msg, "14051") {
			return map[string]any{
				"success":         false,
				"message":         "已领取过专家加油包",
				"already_claimed": true,
			}, nil
		}
		return map[string]any{"success": false, "message": msg}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["success"] = true
	return m, nil
}

// hasTrialPack reports whether the credits summary already contains the
// Global expert trial pack (one-time, 14-day, 250 credits). Used for the
// panel "claim trial" button state (trial_claimed).
//
// Do NOT match bare Chinese "体验": CN free-tier is literally named
// "CodeBuddy个人体验版" / "体验版" and must remain unclaimed-looking for Global
// trial UI (A-18). Prefer English trial markers from live Global packs.
func hasTrialPack(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	for _, p := range cr.Packages {
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if name == "" {
			continue
		}
		// Live Global: "CodeBuddy One-time Free 2-Week Pro Plan Trial"
		if strings.Contains(name, "trial") {
			return true
		}
		// Alternate English shapes (keep without bare "体验")
		if strings.Contains(name, "pro plan") && (strings.Contains(name, "free") || strings.Contains(name, "one-time") || strings.Contains(name, "2-week") || strings.Contains(name, "2 week")) {
			return true
		}
		// Explicit expert-pack Chinese labels only — never bare 体验/体验版.
		if strings.Contains(name, "专家加油") || strings.Contains(name, "专家体验包") {
			return true
		}
	}
	return false
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// Exhausted = we have usage signal and no remaining credits.
// Missing credits data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a package total
	// (used>0, size>0, or packages present). Pure zero with no packages = no data.
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}

func jsonBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case float64:
				return t != 0
			case string:
				return t == "true" || t == "1"
			}
		}
	}
	return false
}

func jsonI64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case int64:
				return t
			case string:
				var n int64
				fmt.Sscanf(t, "%d", &n)
				return n
			}
		}
	}
	return 0
}

func jsonStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}
