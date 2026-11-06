package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// A subprocess isolates a regressed mutex deadlock from the rest of the suite.
// Drive the real reward -> credit refresh -> lifecycle call chain, including
// all three lifecycle branches that acquire the account lock themselves.
func TestRewardExecutionCompletesWithLifecycle(t *testing.T) {
	mode := os.Getenv("WB_REWARD_LIFECYCLE_TEST")
	if mode == "" {
		for _, scenario := range []string{"healthy", "reenable", "exhausted"} {
			t.Run(scenario, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRewardExecutionCompletesWithLifecycle$", "-test.timeout=4s")
				cmd.Env = append(os.Environ(), "WB_REWARD_LIFECYCLE_TEST="+scenario)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("reward execution did not finish: %v\n%s", err, out)
				}
			})
		}
		return
	}

	sa := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}, Account: storedAccount{UID: "reward-test"}}
	f := pluginapi.HostAuthFileEntry{AuthIndex: "reward-test", ID: "reward-test", Name: "workbuddy-reward-test.json"}
	rewardHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) { return []pluginapi.HostAuthFileEntry{f}, nil }
	rewardHostAuthGet = func(string) (*storedAuth, error) { return sa, nil }
	reconcileHostAuthGetBundle = func(string) (*storedAuth, *hostAuthPhysical, error) {
		return sa, &hostAuthPhysical{Disabled: mode == "reenable"}, nil
	}
	lifecycleAuto = true
	cfg := defaultRewardConfig()
	for action := range cfg.Defaults.Actions {
		cfg.Defaults.Actions[action] = action == rewardCheckin
	}
	setRewardConfig(cfg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			fmt.Fprint(w, `{"code":0,"data":{"active":true,"today_checked_in":false}}`)
		case "/v2/billing/meter/daily-checkin":
			fmt.Fprint(w, `{"code":0,"data":{"success":true,"credit":10}}`)
		case "/v2/billing/meter/get-user-resource":
			remain := 10
			if mode == "exhausted" {
				remain = 0
			}
			fmt.Fprintf(w, `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[{"CapacitySize":10,"CapacityRemain":%d}]}}}}`, remain)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	for pass := 0; pass < 2; pass++ {
		out := runAutomaticRewards()
		if out["started"] != 1 {
			t.Fatalf("scheduled pass %d was blocked: %#v", pass, out)
		}
		deadline := time.Now().Add(time.Second)
		for {
			rewardState.Lock()
			running := rewardState.running[f.AuthIndex]
			result := rewardState.results[f.AuthIndex]
			rewardState.Unlock()
			if !running {
				if !result.Success || result.Running || result.Credits != 10 || result.FinishedAt == "" {
					t.Fatalf("incomplete reward result: %#v", result)
				}
				if !strings.Contains(fmt.Sprint(result.Steps), "签到成功") {
					t.Fatalf("check-in result missing: %#v", result)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("successful check-in never completes with lifecycle enabled; later scheduled runs remain blocked")
			}
			time.Sleep(time.Millisecond)
		}
		mu := checkinLockFor(f.AuthIndex)
		if !mu.TryLock() {
			t.Fatal("reward execution retained the account lock")
		}
		mu.Unlock()
	}
}
