// region.go resolves a QoderWork account's realm (CN or international) to the
// endpoint set and request profile that realm uses.
//
// The two realms are entirely separate services:
//
//	CN  (qoder.com.cn):     openapi.qoder.com.cn / gateway.qoder.com.cn
//	                        COSY-signed agent_chat_generation, device grant
//	                        redirect qoder-work-cn://
//	INTL (qoder.com):       openapi.qoder.sh / api2-v2.qoder.sh
//	                        OpenAI-compatible /model/v1/chat/completions with a
//	                        plain Bearer job token, device grant redirect
//	                        qoder://aicoding.aicoding-agent/login-success
//
// Accounts are not shared between realms. The realm is persisted on the auth
// record (storedTokens.Region, falling back to the legacy Domain field) so every
// later call routes to the same service the credential was issued by.
package main

import (
	"errors"
	"strings"
)

type qoderRegion string

const (
	regionCN   qoderRegion = "cn"
	regionIntl qoderRegion = "intl"
)

// qoderEndpointSet is the per-realm service map.
type qoderEndpointSet struct {
	Region      qoderRegion
	Website     string // auth pages (device/selectAccounts)
	OpenAPI     string // token exchange/refresh, userinfo, quota, plan
	Gateway     string // inference + model catalog
	RedirectURI string
	ClientID    string
}

const (
	qoderIntlWebsite = "https://qoder.com"
	qoderIntlOpenAPI = "https://openapi.qoder.sh"
	qoderIntlGateway = "https://api2-v2.qoder.sh"
	// The international desktop client finishes the device grant on this
	// deep link (cockpit-tools uses the same value).
	qoderIntlRedirectURI = "qoder://aicoding.aicoding-agent/login-success"

	// Realm markers persisted in storedTokens.Domain. CN is the historical
	// value; intl is what the international client reports.
	qoderDomainCN   = "qoder.com.cn"
	qoderDomainIntl = "qoder.sh"
)

// qoderIntlGatewayOverride lets tests point the international gateway at a
// local server. Empty in production.
var qoderIntlGatewayOverride = ""

func endpointsForRegion(region qoderRegion) qoderEndpointSet {
	if region == regionIntl {
		gateway := qoderIntlGateway
		if qoderIntlGatewayOverride != "" {
			gateway = qoderIntlGatewayOverride
		}
		return qoderEndpointSet{
			Region:      regionIntl,
			Website:     qoderIntlWebsite,
			OpenAPI:     qoderIntlOpenAPI,
			Gateway:     gateway,
			RedirectURI: qoderIntlRedirectURI,
			ClientID:    qoderClientID,
		}
	}
	return qoderEndpointSet{
		Region:      regionCN,
		Website:     qoderWebsiteCN,
		OpenAPI:     upstreamBaseCN,
		Gateway:     gatewayBaseCN,
		RedirectURI: qoderRedirectURI,
		ClientID:    qoderClientID,
	}
}

// regionFromDomain classifies a stored realm marker. Anything that is not the
// international realm stays CN, which keeps every pre-existing credential (and
// the whole CN flow) on its current path.
func regionFromDomain(domain string) qoderRegion {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return regionCN
	}
	if strings.Contains(d, "qoder.sh") || strings.Contains(d, "qoder.com") && !strings.Contains(d, "qoder.com.cn") {
		return regionIntl
	}
	return regionCN
}

// region reports the realm of a stored credential.
func (sa *storedAuth) region() qoderRegion {
	if sa == nil {
		return regionCN
	}
	if r := strings.TrimSpace(sa.Auth.Region); r != "" {
		if qoderRegion(r) == regionIntl {
			return regionIntl
		}
		return regionCN
	}
	return regionFromDomain(sa.Auth.Domain)
}

// endpoints reports the endpoint set for a stored credential.
func (sa *storedAuth) endpoints() qoderEndpointSet {
	return endpointsForRegion(sa.region())
}

// isCN reports whether the credential is a CN (qoder.com.cn) account. CN-only
// features (check-in, credits panel, sash endpoints) are gated on this.
func (sa *storedAuth) isCN() bool { return sa.region() == regionCN }

// parseRegionValue normalises a panel-supplied region selector. Anything other
// than an explicit international value stays CN, which preserves the historical
// single-region behaviour for existing callers that never send a region.
func parseRegionValue(v string) qoderRegion {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "intl", "international", "global", "qoder.sh":
		return regionIntl
	default:
		return regionCN
	}
}

// serviceNameForRegion is the panel's opaque service label for a realm.
func serviceNameForRegion(r qoderRegion) string {
	if r == regionIntl {
		return "qoder-intl"
	}
	return "qoder-cn"
}

// domainForRegion is the realm marker persisted on a credential so later calls
// route to the same service.
func domainForRegion(r qoderRegion) string {
	if r == regionIntl {
		return qoderDomainIntl
	}
	return qoderDomainCN
}

// errCheckinUnsupported marks a CN-only feature requested for an international
// account. The /sash/* reward endpoints (daily check-in, Pro upgrade pack) do
// not exist on the international realm.
var errCheckinUnsupported = errors.New("签到/奖励功能仅支持国内版（qoder.com.cn）账号")
