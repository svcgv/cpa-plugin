package main

import "testing"

func TestServiceFromDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   wbService
	}{
		{"www.codebuddy.cn", serviceCN},
		{"codebuddy.cn", serviceCN},
		{"copilot.tencent.com", serviceCN},
		{"", serviceCN},
		{"   ", serviceCN},
		{"unknown.example", serviceCN},
		{"www.codebuddy.ai", serviceCodeBuddyGlobal},
		{"codebuddy.ai", serviceCodeBuddyGlobal},
		{"CODEBUDDY.AI", serviceCodeBuddyGlobal},
		{"  www.codebuddy.ai  ", serviceCodeBuddyGlobal},
		{"www.workbuddy.ai", serviceWorkBuddyGlobal},
		{"workbuddy.ai", serviceWorkBuddyGlobal},
		// Substring matches must not leak a domain into another service.
		{"evilcodebuddy.ai", serviceCN},
		{"codebuddy.ai.evil.com", serviceCN},
		{"notworkbuddy.ai", serviceCN},
		{"evilworkbuddy.ai", serviceCN},
		{"workbuddy.ai.evil.com", serviceCN},
	}
	for _, tc := range cases {
		if got := serviceFromDomain(tc.domain); got != tc.want {
			t.Errorf("serviceFromDomain(%q) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

// Two international services must never collapse into one another: they have
// different hosts, and a credential from one is rejected by the other.
func TestServiceForDistinguishesInternationalServices(t *testing.T) {
	cb := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}}
	wb := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
	if serviceFor(cb) == serviceFor(wb) {
		t.Fatalf("codebuddy.ai and workbuddy.ai resolved to the same service %q", serviceFor(cb))
	}
	if serviceFor(cb) != serviceCodeBuddyGlobal {
		t.Fatalf("codebuddy.ai service = %q", serviceFor(cb))
	}
	if serviceFor(wb) != serviceWorkBuddyGlobal {
		t.Fatalf("workbuddy.ai service = %q", serviceFor(wb))
	}
}

func TestServiceEndpoints(t *testing.T) {
	cases := []struct {
		name       string
		sa         *storedAuth
		upstream   string
		billing    string
		origin     string
		region     string
		serviceStr string
	}{
		{
			name:       "cn",
			sa:         &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}},
			upstream:   upstreamBaseCN,
			billing:    billingBase,
			origin:     originReferer,
			region:     "cn",
			serviceStr: "cn",
		},
		{
			name:       "codebuddy global",
			sa:         &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}},
			upstream:   upstreamBaseCodeBuddyGlobal,
			billing:    billingBaseCodeBuddyGlobal,
			origin:     originRefererCodeBuddyGlobal,
			region:     "global",
			serviceStr: "codebuddy-global",
		},
		{
			name:       "workbuddy global",
			sa:         &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}},
			upstream:   upstreamBaseGlobal,
			billing:    billingBaseGlobal,
			origin:     originRefererGlobal,
			region:     "global",
			serviceStr: "workbuddy-global",
		},
		{
			name:       "legacy empty domain defaults to cn",
			sa:         &storedAuth{},
			upstream:   upstreamBaseCN,
			billing:    billingBase,
			origin:     originReferer,
			region:     "cn",
			serviceStr: "cn",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamBaseFor(tc.sa); got != tc.upstream {
				t.Errorf("upstreamBaseFor = %q, want %q", got, tc.upstream)
			}
			if got := billingBaseFor(tc.sa); got != tc.billing {
				t.Errorf("billingBaseFor = %q, want %q", got, tc.billing)
			}
			if got := originRefererFor(tc.sa); got != tc.origin {
				t.Errorf("originRefererFor = %q, want %q", got, tc.origin)
			}
			if got := accountRegion(tc.sa); got != tc.region {
				t.Errorf("accountRegion = %q, want %q", got, tc.region)
			}
			if got := string(serviceFor(tc.sa)); got != tc.serviceStr {
				t.Errorf("serviceFor = %q, want %q", got, tc.serviceStr)
			}
		})
	}
}

// A CodeBuddy-international credential must not be routed at the WorkBuddy
// international host, and vice versa: the tokens are realm-scoped.
func TestUpstreamHostsDoNotCrossServices(t *testing.T) {
	cb := upstreamBaseFor(&storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}})
	wb := upstreamBaseFor(&storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}})
	cn := upstreamBaseFor(&storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}})
	seen := map[string]string{cb: "codebuddy-global", wb: "workbuddy-global", cn: "cn"}
	if len(seen) != 3 {
		t.Fatalf("services share an upstream host: %v", seen)
	}
}

func TestRealmForServiceIsDistinct(t *testing.T) {
	realms := map[workBuddyRealm]wbService{
		realmForService(serviceCN):              serviceCN,
		realmForService(serviceCodeBuddyGlobal): serviceCodeBuddyGlobal,
		realmForService(serviceWorkBuddyGlobal): serviceWorkBuddyGlobal,
	}
	if len(realms) != 3 {
		t.Fatalf("services share a model realm: %v", realms)
	}
	for realm := range realms {
		if !realm.valid() {
			t.Errorf("realm %q reported invalid", realm)
		}
	}
}

func TestAuthFileNamesPerService(t *testing.T) {
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	sa := &storedAuth{Account: storedAccount{UID: uid}}

	cn := authFileNameForService(sa, serviceCN)
	wb := authFileNameForService(sa, serviceWorkBuddyGlobal)
	cb := authFileNameForService(sa, serviceCodeBuddyGlobal)

	if cn != "workbuddy-"+uid+".json" {
		t.Errorf("cn file name = %q", cn)
	}
	// Existing WorkBuddy-international files keep their historical name so the
	// host's stored identity is preserved across upgrades.
	if wb != cn {
		t.Errorf("workbuddy-global file name = %q, want %q (unchanged)", wb, cn)
	}
	// CodeBuddy international needs a distinct name because the same UID can
	// exist on both services.
	if cb == cn {
		t.Errorf("codebuddy-global file name collides with cn: %q", cb)
	}
	if cb != codeBuddyGlobalFilePrefix+uid+".json" {
		t.Errorf("codebuddy-global file name = %q", cb)
	}
	// authFileNameFor must keep resolving by the credential's own service.
	if got := authFileNameFor(sa); got != cn {
		t.Errorf("authFileNameFor(empty domain) = %q, want %q", got, cn)
	}
	cbSA := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}, Account: storedAccount{UID: uid}}
	if got := authFileNameFor(cbSA); got != cb {
		t.Errorf("authFileNameFor(codebuddy.ai) = %q, want %q", got, cb)
	}
}
