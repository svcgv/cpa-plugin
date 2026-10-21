package main

import (
	"encoding/json"
	"testing"
)

func parseQuota(t *testing.T, raw string) *creditsSummary {
	t.Helper()
	var q quotaUsageResponse
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return summarizeQuotaUsage(q)
}

func TestSummarizeQuotaUsageKeepsPlanAndResourcePackSeparate(t *testing.T) {
	cr := parseQuota(t, `{
		"userId":"u1","userType":"personal_pro","usageType":"credits",
		"userQuota":{"total":300,"used":200,"remaining":100,"percentage":0.66,"unit":"credits"},
		"addOnQuota":{"total":300,"used":0,"remaining":300,"percentage":0,"unit":"credits","detailUrl":"https://qoder.com.cn/account/usage"}
	}`)

	if cr.PlanRemain != 100 || cr.PlanUsed != 200 || cr.PlanSize != 300 {
		t.Fatalf("plan pool = remain %d used %d size %d, want 100/200/300", cr.PlanRemain, cr.PlanUsed, cr.PlanSize)
	}
	if cr.PackRemain != 300 || cr.PackUsed != 0 || cr.PackSize != 300 {
		t.Fatalf("resource pack = remain %d used %d size %d, want 300/0/300", cr.PackRemain, cr.PackUsed, cr.PackSize)
	}
	// The aggregate stays available for exhaustion decisions, but the panel
	// must render PlanRemain and PackRemain as separate rows.
	if cr.TotalRemain != 400 || cr.TotalUsed != 200 || cr.TotalSize != 600 {
		t.Fatalf("aggregate = remain %d used %d size %d, want 400/200/600", cr.TotalRemain, cr.TotalUsed, cr.TotalSize)
	}
}

func TestSummarizeQuotaUsageAcceptsSnakeCaseAndOrgPool(t *testing.T) {
	cr := parseQuota(t, `{
		"user_id":"u2","user_type":"team_member","usage_type":"credits",
		"user_quota":{"total":100,"used":0,"remaining":100,"unit":"credits"},
		"add_on_quota":{"total":0,"used":0,"remaining":0,"unit":"credits"},
		"org_resource_package":{"cap":300,"used":50,"remaining":250,"available":true,"unit":"credits"}
	}`)

	if cr.PlanRemain != 100 || cr.PackRemain != 0 {
		t.Fatalf("plan/pack = %d/%d, want 100/0", cr.PlanRemain, cr.PackRemain)
	}
	if !cr.SharedAvailable || cr.SharedRemain != 250 || cr.SharedSize != 300 || cr.SharedUsed != 50 {
		t.Fatalf("shared pool = available %v remain %d size %d used %d", cr.SharedAvailable, cr.SharedRemain, cr.SharedSize, cr.SharedUsed)
	}
	if cr.TotalRemain != 350 {
		t.Fatalf("total remain = %d, want 350 (plan 100 + shared 250)", cr.TotalRemain)
	}
}

func TestSummarizeQuotaUsageUnavailableOrgPoolIsNotSpendable(t *testing.T) {
	cr := parseQuota(t, `{
		"userQuota":{"total":0,"used":0,"remaining":0},
		"addOnQuota":{"total":100,"used":0,"remaining":100},
		"orgResourcePackage":{"cap":500,"used":0,"remaining":500,"available":false}
	}`)

	if cr.SharedAvailable {
		t.Fatal("shared pool should be marked unavailable")
	}
	if cr.TotalRemain != 100 {
		t.Fatalf("total remain = %d, want 100 (unavailable shared pool excluded)", cr.TotalRemain)
	}
	if cr.PackRemain != 100 {
		t.Fatalf("resource pack remain = %d, want 100", cr.PackRemain)
	}
}

func TestUsagePresentationUnwrapsQoderUsage(t *testing.T) {
	var p usagePresentationResponse
	if err := json.Unmarshal([]byte(`{
		"displayMode":"qoder",
		"qoderUsage":{
			"userType":"team_member","usageType":"credits",
			"userQuota":{"total":100,"used":0,"remaining":100,"unit":"credits"},
			"addOnQuota":{"total":300,"used":0,"remaining":300,"unit":"credits"}
		}
	}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.DisplayMode != "qoder" || p.QoderUsage == nil {
		t.Fatalf("presentation = %q / %v", p.DisplayMode, p.QoderUsage)
	}
	cr := summarizeQuotaUsage(*p.QoderUsage)
	if cr.PlanRemain != 100 || cr.PackRemain != 300 || cr.TotalRemain != 400 {
		t.Fatalf("plan/pack/total = %d/%d/%d, want 100/300/400", cr.PlanRemain, cr.PackRemain, cr.TotalRemain)
	}
}
