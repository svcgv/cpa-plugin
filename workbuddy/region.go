// region.go resolves which CodeBuddy/WorkBuddy service an account belongs to and
// maps that service onto its upstream hosts, billing base and panel region tag.
//
// Three services exist and never share credentials or endpoints:
//
//	cn               www.codebuddy.cn  / copilot.tencent.com  (Keycloak CN realm)
//	codebuddy-global www.codebuddy.ai                          (CodeBuddy international)
//	workbuddy-global www.workbuddy.ai                          (legacy WorkBuddy international)
//
// Routing is derived from the stored credential domain, so an auth file written
// by an older plugin version (domain "www.workbuddy.ai") keeps its original
// service instead of being reinterpreted as CodeBuddy international.
package main

import "strings"

// wbService identifies the CodeBuddy/WorkBuddy service backing an account.
type wbService string

const (
	serviceCN              wbService = "cn"
	serviceCodeBuddyGlobal wbService = "codebuddy-global"
	serviceWorkBuddyGlobal wbService = "workbuddy-global"
)

// International (non-CN) hosts. upstreamBaseCN lives in main.go alongside the
// CN endpoint aliases.
const (
	upstreamBaseCodeBuddyGlobal  = "https://www.codebuddy.ai"
	originRefererCodeBuddyGlobal = "https://www.codebuddy.ai"
)

// Billing bases. Vars (not consts) so tests can point them at httptest servers.
var (
	billingBase                = "https://www.codebuddy.cn"
	billingBaseCodeBuddyGlobal = "https://www.codebuddy.ai"
	// billingBaseGlobal is the legacy WorkBuddy international billing base.
	billingBaseGlobal = "https://www.workbuddy.ai"
)

// serviceFromDomain maps a stored credential domain onto its service. Unknown
// or empty domains fall back to CN, matching the pre-existing behaviour for
// legacy auth files that predate the domain field.
func serviceFromDomain(domain string) wbService {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch {
	case d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai"):
		return serviceCodeBuddyGlobal
	case d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai"):
		return serviceWorkBuddyGlobal
	default:
		return serviceCN
	}
}

// serviceFor resolves the service for one stored auth.
func serviceFor(sa *storedAuth) wbService {
	if sa == nil {
		return serviceCN
	}
	return serviceFromDomain(sa.Auth.Domain)
}

// region is the panel-facing tag: "cn" or "global".
func (s wbService) region() string {
	if s == serviceCN {
		return "cn"
	}
	return "global"
}

// label is the human-readable service name shown in notes and the panel.
func (s wbService) label() string {
	switch s {
	case serviceCodeBuddyGlobal:
		return "Global(CB)"
	case serviceWorkBuddyGlobal:
		return "Global(WB)"
	default:
		return "CN"
	}
}

// isGlobalDomain reports whether a domain belongs to either international
// service. Substring matches are rejected so "evilworkbuddy.ai" is not global.
func isGlobalDomain(domain string) bool {
	return serviceFromDomain(domain) != serviceCN
}

// accountRegion returns "cn" or "global" based on the auth's service.
func accountRegion(sa *storedAuth) string {
	return serviceFor(sa).region()
}

// billingBaseFor returns the billing API base for the account's service.
func billingBaseFor(sa *storedAuth) string {
	switch serviceFor(sa) {
	case serviceCodeBuddyGlobal:
		return billingBaseCodeBuddyGlobal
	case serviceWorkBuddyGlobal:
		return billingBaseGlobal
	default:
		return billingBase
	}
}

// originRefererFor returns the Origin/Referer base for the account's service.
func originRefererFor(sa *storedAuth) string {
	switch serviceFor(sa) {
	case serviceCodeBuddyGlobal:
		return originRefererCodeBuddyGlobal
	case serviceWorkBuddyGlobal:
		return originRefererGlobal
	default:
		return originReferer
	}
}

// upstreamBaseFor returns the chat/auth API host for the account's service.
// JWTs are realm-scoped: a CN token is rejected by the international gateways
// and vice versa, so mixing services yields APISIX 401.
func upstreamBaseFor(sa *storedAuth) string {
	switch serviceFor(sa) {
	case serviceCodeBuddyGlobal:
		return upstreamBaseCodeBuddyGlobal
	case serviceWorkBuddyGlobal:
		return upstreamBaseGlobal
	default:
		return upstreamBaseCN
	}
}

// refreshDomainFor returns the X-Domain value for the account's service. It is
// the stored credential domain when present so an upstream-issued value wins,
// otherwise the canonical host of the service.
func refreshDomainFor(sa *storedAuth) string {
	if sa != nil && strings.TrimSpace(sa.Auth.Domain) != "" {
		return strings.TrimSpace(sa.Auth.Domain)
	}
	switch serviceFor(sa) {
	case serviceCodeBuddyGlobal:
		return "www.codebuddy.ai"
	case serviceWorkBuddyGlobal:
		return "www.workbuddy.ai"
	default:
		return "www.codebuddy.cn"
	}
}

// -----------------------------------------------------------------------------
// Test seam
// -----------------------------------------------------------------------------

// setBillingBase temporarily overrides the CN billing base; returns a restore
// func. Used by tests that point billing at an httptest server.
func setBillingBase(s string) func() {
	old := billingBase
	billingBase = s
	return func() { billingBase = old }
}

// setBillingBaseGlobal temporarily overrides the legacy WorkBuddy
// international billing base.
func setBillingBaseGlobal(s string) func() {
	old := billingBaseGlobal
	billingBaseGlobal = s
	return func() { billingBaseGlobal = old }
}

// setBillingBaseCodeBuddyGlobal temporarily overrides the CodeBuddy
// international billing base.
func setBillingBaseCodeBuddyGlobal(s string) func() {
	old := billingBaseCodeBuddyGlobal
	billingBaseCodeBuddyGlobal = s
	return func() { billingBaseCodeBuddyGlobal = old }
}
