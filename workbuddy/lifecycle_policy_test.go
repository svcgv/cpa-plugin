package main

import "testing"

func exhaustedCredits() *creditsSummary {
	return &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100}
}

func healthyCredits() *creditsSummary {
	return &creditsSummary{TotalRemain: 50, TotalUsed: 50, TotalSize: 100}
}

// An account whose quota can recover must be disabled, never deleted, because
// deletion is irreversible and forces the user to re-authorize.
func TestLifecycleActionKeepsRecoverableAccounts(t *testing.T) {
	cases := []struct {
		name       string
		service    wbService
		capability checkinCapability
		want       lifecycleAction
	}{
		{"cn never deletes", serviceCN, capabilitySupported, lifecycleDisable},
		{"cn with unknown capability", serviceCN, capabilityUnknown, lifecycleDisable},
		{"codebuddy global with check-in activity", serviceCodeBuddyGlobal, capabilitySupported, lifecycleDisable},
		{"codebuddy global with unknown capability", serviceCodeBuddyGlobal, capabilityUnknown, lifecycleDisable},
		{"codebuddy global with no activity is a one-shot", serviceCodeBuddyGlobal, capabilityUnsupported, lifecycleDelete},
		{"workbuddy global with check-in activity", serviceWorkBuddyGlobal, capabilitySupported, lifecycleDisable},
		{"workbuddy global with unknown capability", serviceWorkBuddyGlobal, capabilityUnknown, lifecycleDisable},
		{"workbuddy global with no activity is a one-shot", serviceWorkBuddyGlobal, capabilityUnsupported, lifecycleDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lifecycleActionForService(tc.service, tc.capability, exhaustedCredits())
			if got != tc.want {
				t.Fatalf("action = %v, want %v", got, tc.want)
			}
		})
	}
}

// Unknown credits must never trigger an action: a transient billing failure is
// not evidence of exhaustion.
func TestLifecycleActionIgnoresUnknownCredits(t *testing.T) {
	for _, cr := range []*creditsSummary{nil, {}, {Packages: nil}} {
		for _, service := range []wbService{serviceCN, serviceCodeBuddyGlobal, serviceWorkBuddyGlobal} {
			if got := lifecycleActionForService(service, capabilityUnsupported, cr); got != lifecycleNone {
				t.Fatalf("service %q with credits %#v: action = %v, want none", service, cr, got)
			}
		}
	}
}

func TestCheckinCapabilityFromSummary(t *testing.T) {
	cases := []struct {
		name string
		ci   *checkinSummary
		want checkinCapability
	}{
		{"nil summary", nil, capabilityUnknown},
		{"missing verdict", &checkinSummary{}, capabilityUnknown},
		{"supported", &checkinSummary{Capability: "supported"}, capabilitySupported},
		{"unsupported", &checkinSummary{Capability: "unsupported"}, capabilityUnsupported},
		{"garbage verdict", &checkinSummary{Capability: "weird"}, checkinCapability("weird")},
	}
	for _, tc := range cases {
		if got := checkinCapabilityFromSummary(tc.ci); got != tc.want {
			t.Errorf("%s: capability = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A disabled international account that regains credits must be re-enabled:
// it is no longer deleted, so this is the only path back to service.
func TestShouldReenableCoversEveryService(t *testing.T) {
	if !shouldReenable(true, healthyCredits()) {
		t.Error("healthy credits on a disabled account must re-enable it")
	}
	if shouldReenable(false, healthyCredits()) {
		t.Error("an enabled account needs no re-enable")
	}
	if shouldReenable(true, exhaustedCredits()) {
		t.Error("still-exhausted credits must not re-enable")
	}
	if shouldReenable(true, nil) {
		t.Error("unknown credits must not re-enable")
	}
}

// The display note and host label must name the actual service, so two
// international accounts are distinguishable in the auth list.
func TestServiceLabelsAreDistinct(t *testing.T) {
	cn := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	cb := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}}
	wb := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
	seen := map[string]bool{}
	for _, sa := range []*storedAuth{cn, cb, wb} {
		label := serviceFor(sa).label()
		if seen[label] {
			t.Errorf("label %q is not unique across services", label)
		}
		seen[label] = true
		if note := displayNote(sa, healthyCredits(), false); note == "" {
			t.Errorf("empty note for %q", label)
		}
		if l := labelForAuth(sa); l == "" {
			t.Errorf("empty host label for %q", label)
		}
	}
}
