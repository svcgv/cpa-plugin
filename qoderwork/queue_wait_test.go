package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQueueHintNestedAndNonQueue(t *testing.T) {
	inner, _ := json.Marshal(map[string]any{"isQueued": true, "queueType": "p3", "retryAfterSeconds": 30})
	middle, _ := json.Marshal(map[string]any{"code": "10605", "message": string(inner)})
	outer, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": string(middle)})
	hint, ok := parseQoderQueueHint(outer)
	if !ok || hint.queueType != "p3" || hint.retry != 30*time.Second {
		t.Fatalf("queue hint: %#v %v", hint, ok)
	}
	if _, ok := parseQoderQueueHint([]byte(`{"code":401,"message":"expired"}`)); ok {
		t.Fatal("auth error treated as queue")
	}
	split, _ := json.Marshal(map[string]any{"code": "10605", "message": `{"retryAfterSeconds":7,"queueType":"p1"}`})
	if hint, ok := parseQoderQueueHint(split); !ok || hint.retry != 7*time.Second || hint.queueType != "p1" {
		t.Fatalf("split nested hint: %#v %v", hint, ok)
	}
}

func TestQueueInspectionDoesNotReplayAfterFirstContent(t *testing.T) {
	input := "data: {\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"hello\\\"}}]}\"}\n\ndata: {\"statusCodeValue\":403,\"body\":\"{\\\"isQueued\\\":true,\\\"queueCount\\\":1,\\\"retryAfterSeconds\\\":30}\"}\n\n"
	stream := &hostHTTPStream{direct: []byte(input)}
	inspected, _, queued, err := inspectQueueStream(stream, 200)
	if err != nil || queued {
		t.Fatalf("first content treated as queue: %v %v", queued, err)
	}
	defer inspected.Close()
	rest, err := io.ReadAll(newHostStreamReader(inspected))
	if err != nil || string(rest) != input {
		t.Fatal("first content or remaining bytes lost")
	}
}

func TestWaitQueuePollingAndCancellation(t *testing.T) {
	count := 0
	delays := []time.Duration{}
	poll := func(context.Context) (qoderQueueStatus, error) {
		count++
		if count == 1 {
			return qoderQueueStatus{IsQueued: true}, nil
		}
		return qoderQueueStatus{IsQueued: false}, nil
	}
	sleep := func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	ready, err := waitQoderQueue(context.Background(), time.Now().Add(time.Minute), qoderQueueHint{retry: time.Second}, poll, sleep, 2*time.Second, 30*time.Second, nil)
	if err != nil || !ready || count != 2 || len(delays) != 2 || delays[0] != 2*time.Second {
		t.Fatalf("queue: %v %v %d %v", ready, err, count, delays)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = waitQoderQueue(ctx, time.Now().Add(time.Minute), qoderQueueHint{}, poll, sleep, time.Second, time.Second, nil); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestQueuePollReplayPreservesRequestSet(t *testing.T) {
	old := currentQueueSettings()
	setQueueSettings(true, 3)
	t.Cleanup(func() { setQueueSettings(old.enabled, int(old.budget/time.Second)) })
	var bodies []map[string]any
	polls := 0
	qoderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat":
			data, _ := io.ReadAll(r.Body)
			decoded, err := qoderDecode(data)
			if err != nil {
				t.Error(err)
				return
			}
			var body map[string]any
			_ = json.Unmarshal(decoded, &body)
			bodies = append(bodies, body)
			if len(bodies) == 1 {
				inner, _ := json.Marshal(map[string]any{"isQueued": true, "queueType": "p3", "retryAfterSeconds": 0})
				outer, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": string(inner)})
				fmt.Fprintf(w, "data: %s\n\n", outer)
				return
			}
			fmt.Fprint(w, "data: {\"body\":\"[DONE]\"}\n\n")
		case "/algo/api/v2/service/ask/queue/status":
			polls++
			if r.URL.Query().Get("requestSetId") != "fixed-set" || r.URL.Query().Get("modelKey") != "qfmodel" {
				t.Error("queue identity mismatch")
			}
			if r.Header.Get("X-Request-Id") != "first-id" || r.Header.Get("Cosy-Machinetype") != "5" || r.Header.Get("Cosy-Bodylength") != "0" {
				t.Error("reference queue correlation or COSY headers missing")
			}
			fmt.Fprint(w, `{"isQueued":false,"retryAfterSeconds":0}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	sa := &storedAuth{Auth: storedTokens{AccessToken: "jt-test", PersonalToken: "pt-test-queue", Region: "cn"}, Account: storedAccount{UID: "queue-user"}}
	initial := map[string]any{"request_id": "first-id", "chat_record_id": "first-id", "request_set_id": "fixed-set", "business": map[string]any{"id": "fixed-set"}}
	raw, _ := json.Marshal(initial)
	encoded := qoderEncode(raw)
	req, _ := http.NewRequest(http.MethodPost, "https://gateway.qoder.com.cn/chat", strings.NewReader(encoded))
	if err := applyCosyHeaders(req, sa, encoded, req.URL.String(), "qfmodel", true); err != nil {
		t.Fatal(err)
	}
	stream, status, _, err := openChatWithQueue(req, sa, encoded, "qfmodel", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	result, _ := io.ReadAll(newHostStreamReader(stream))
	if status != 200 || !bytes.Contains(result, []byte("[DONE]")) {
		t.Fatalf("replay status=%d body=%s", status, result)
	}
	if polls != 1 || len(bodies) != 2 || bodies[0]["request_set_id"] != bodies[1]["request_set_id"] || bodies[0]["request_id"] == bodies[1]["request_id"] {
		t.Fatalf("queue replay identity failed; polls=%d attempts=%d", polls, len(bodies))
	}
}
