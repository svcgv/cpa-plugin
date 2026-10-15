package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// checkinServer answers the two candidate check-in endpoints with the supplied
// handler. Paths can return per-endpoint statuses via the map.
func checkinServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.URL.Path]; ok {
			h(w, r)
			return
		}
		// Any other path (credits, payment type, ...) is not under test.
		http.NotFound(w, r)
	}))
}

func jsonEnvelope(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":` + data + `}`))
}

func TestCheckinCapabilitySupportedOnActivityEndpoint(t *testing.T) {
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			jsonEnvelope(w, `{"active":true,"today_checked_in":false,"daily_credit":100,"streak_days":3}`)
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilitySupported {
		t.Fatalf("capability = %q, want supported (message=%q)", probe.Capability, probe.Message)
	}
	if probe.Summary == nil || probe.Summary.DailyCredit != 100 || probe.Summary.StreakDays != 3 {
		t.Fatalf("summary = %#v", probe.Summary)
	}
}

func TestCheckinCapabilityFallsBackToLegacyStatusOnlyOnMissingEndpoint(t *testing.T) {
	var legacyCalls int
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		"/v2/billing/meter/checkin-status": func(w http.ResponseWriter, r *http.Request) {
			legacyCalls++
			jsonEnvelope(w, `{"active":true,"todayCheckedIn":true}`)
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilitySupported {
		t.Fatalf("capability = %q, want supported", probe.Capability)
	}
	if legacyCalls != 1 {
		t.Fatalf("legacy endpoint calls = %d, want 1", legacyCalls)
	}
	if probe.Summary == nil || !probe.Summary.TodayCheckedIn {
		t.Fatalf("summary = %#v", probe.Summary)
	}
}

// A 401/429/5xx from the activity endpoint is an inconclusive probe, not proof
// that the account has no activity.
func TestCheckinCapabilityUnknownOnNon404Failures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var legacyCalls int
			srv := checkinServer(t, map[string]http.HandlerFunc{
				"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
				},
				"/v2/billing/meter/checkin-status": func(w http.ResponseWriter, r *http.Request) {
					legacyCalls++
					jsonEnvelope(w, `{"active":true}`)
				},
			})
			defer srv.Close()
			defer setBillingBase(srv.URL)()

			probe := checkinCapabilityProbe(&storedAuth{}, "")
			if probe.Capability != capabilityUnknown {
				t.Fatalf("capability = %q, want unknown (message=%q)", probe.Capability, probe.Message)
			}
			if legacyCalls != 0 {
				t.Fatalf("legacy endpoint must not be probed after a %d, got %d calls", status, legacyCalls)
			}
		})
	}
}

// A body that cannot be parsed must be unknown: the account might well have an
// activity, we simply could not tell.
func TestCheckinCapabilityUnknownOnMalformedBody(t *testing.T) {
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":"not-an-object"}`))
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilityUnknown {
		t.Fatalf("capability = %q, want unknown", probe.Capability)
	}
}

// An explicit active:false is a real "no activity" verdict.
func TestCheckinCapabilityUnsupportedOnExplicitInactive(t *testing.T) {
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			jsonEnvelope(w, `{"active":false}`)
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilityUnsupported {
		t.Fatalf("capability = %q, want unsupported", probe.Capability)
	}
}

// A payload without any activity flag is inconclusive. Treating it as
// unsupported would hide a working activity behind a schema change.
func TestCheckinCapabilityUnknownWhenActivityFlagAbsent(t *testing.T) {
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			jsonEnvelope(w, `{"streak_days":2}`)
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilityUnknown {
		t.Fatalf("capability = %q, want unknown", probe.Capability)
	}
}

// Both candidate endpoints missing means the service has no check-in API.
func TestCheckinCapabilityUnsupportedWhenBothEndpointsMissing(t *testing.T) {
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		"/v2/billing/meter/checkin-status": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilityUnsupported {
		t.Fatalf("capability = %q, want unsupported (message=%q)", probe.Capability, probe.Message)
	}
}

// The check-in call must never turn an explicit upstream failure into success.
func TestPerformCheckinCallPreservesExplicitFailure(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantOK    bool
		wantValue any
	}{
		{"explicit false bool", `{"success":false,"message":"not eligible"}`, false, false},
		{"explicit false string", `{"success":"false","message":"not eligible"}`, false, false},
		{"explicit true bool", `{"success":true}`, true, true},
		{"missing success means the call succeeded", `{"credit":100}`, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := checkinServer(t, map[string]http.HandlerFunc{
				"/v2/billing/meter/daily-checkin": func(w http.ResponseWriter, r *http.Request) {
					jsonEnvelope(w, tc.body)
				},
			})
			defer srv.Close()
			defer setBillingBase(srv.URL)()

			res, err := performCheckinCall(&storedAuth{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res["success"] != tc.wantValue {
				t.Fatalf("success = %#v, want %#v", res["success"], tc.wantValue)
			}
		})
	}
}

// parseCheckinSummary must distinguish "flag absent" from "flag false".
func TestParseCheckinSummaryActivityFlagTriState(t *testing.T) {
	for _, tc := range []struct {
		body        string
		wantActive  bool
		wantPresent bool
	}{
		{`{"active":true}`, true, true},
		{`{"active":false}`, false, true},
		{`{"Active":1}`, true, true},
		{`{"streak_days":1}`, false, false},
	} {
		sum, err := parseCheckinSummary(json.RawMessage(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if sum.Active != tc.wantActive || sum.activityFlagPresent != tc.wantPresent {
			t.Errorf("%s: active=%v present=%v, want active=%v present=%v",
				tc.body, sum.Active, sum.activityFlagPresent, tc.wantActive, tc.wantPresent)
		}
	}
}

// checkinOneAccount must report "unsupported" without calling daily-checkin,
// so an account with no activity never produces a spurious upstream error.
func TestCheckinOneAccountSkipsUnsupportedWithoutCallingDailyCheckin(t *testing.T) {
	var dailyCalls int
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			jsonEnvelope(w, `{"active":false}`)
		},
		"/v2/billing/meter/checkin-status": func(w http.ResponseWriter, r *http.Request) {
			jsonEnvelope(w, `{"active":false}`)
		},
		"/v2/billing/meter/daily-checkin": func(w http.ResponseWriter, r *http.Request) {
			dailyCalls++
			jsonEnvelope(w, `{"success":true}`)
		},
	})
	defer srv.Close()

	sa := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}, Account: storedAccount{UID: "u1"}}
	res := checkinAccountAgainstServer(t, sa, srv)
	if dailyCalls != 0 {
		t.Fatalf("daily-checkin called %d times for an unsupported account", dailyCalls)
	}
	if res["reason"] != "unsupported" {
		t.Fatalf("reason = %#v, want unsupported", res["reason"])
	}
	if res["success"] == true {
		t.Fatalf("unsupported check-in must not report success")
	}
}

// checkinAccountAgainstServer runs the real single-account check-in path with
// the host credential lookup stubbed to return sa.
func checkinAccountAgainstServer(t *testing.T, sa *storedAuth, srv *httptest.Server) map[string]any {
	t.Helper()
	old := checkinHostAuthGet
	checkinHostAuthGet = func(string) (*storedAuth, error) { return sa, nil }
	t.Cleanup(func() { checkinHostAuthGet = old })
	switch serviceFor(sa) {
	case serviceCodeBuddyGlobal:
		defer setBillingBaseCodeBuddyGlobal(srv.URL)()
	case serviceWorkBuddyGlobal:
		defer setBillingBaseGlobal(srv.URL)()
	default:
		defer setBillingBase(srv.URL)()
	}
	return checkinOneAccountWithCallback(pluginapi.HostAuthFileEntry{
		AuthIndex: "auth-1",
		ID:        "auth-1",
		Name:      "workbuddy-" + sa.Account.UID + ".json",
	}, "")
}

// An inconclusive probe still allows the (idempotent) manual attempt, so a probe
// outage does not block the user. The upstream "already checked in" response is
// what decides the outcome.
func TestCheckinUnknownProbeStillAttemptsAndHonoursUpstreamAlready(t *testing.T) {
	srv := checkinServer(t, map[string]http.HandlerFunc{
		"/v2/billing/meter/checkin-activity-status": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"/v2/billing/meter/daily-checkin": func(w http.ResponseWriter, r *http.Request) {
			jsonEnvelope(w, `{"success":false,"message":"already checked in today"}`)
		},
	})
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	probe := checkinCapabilityProbe(&storedAuth{}, "")
	if probe.Capability != capabilityUnknown {
		t.Fatalf("probe capability = %q, want unknown", probe.Capability)
	}
	res, err := performCheckinCall(&storedAuth{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(strings.ToLower(msg), "already") {
		t.Fatalf("message = %q", msg)
	}
}
