package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type queueSettings struct {
	enabled bool
	budget  time.Duration
}

var queueConfig = struct {
	sync.RWMutex
	value queueSettings
}{value: queueSettings{true, 540 * time.Second}}

func setQueueSettings(enabled bool, seconds int) {
	queueConfig.Lock()
	queueConfig.value = queueSettings{enabled, time.Duration(seconds) * time.Second}
	queueConfig.Unlock()
}
func currentQueueSettings() queueSettings {
	queueConfig.RLock()
	defer queueConfig.RUnlock()
	return queueConfig.value
}

type qoderQueueHint struct {
	queueType string
	retry     time.Duration
}
type qoderQueueStatus struct {
	IsQueued          bool  `json:"isQueued"`
	ServiceAvailable  *bool `json:"serviceAvailable"`
	QueueCount        int   `json:"queueCount"`
	RetryAfterSeconds *int  `json:"retryAfterSeconds"`
	WaitTime          int   `json:"waitTime"`
}

type queueProgressEntry struct {
	Model       string `json:"model"`
	Region      string `json:"region"`
	QueueType   string `json:"queue_type,omitempty"`
	QueueCount  int    `json:"queue_count"`
	WaitSeconds int    `json:"wait_seconds"`
	UpdatedAt   string `json:"updated_at"`
}

var queueProgress sync.Map

func queueProgressSnapshot() []queueProgressEntry {
	entries := make([]queueProgressEntry, 0)
	queueProgress.Range(func(_, value any) bool {
		if row, ok := value.(queueProgressEntry); ok {
			entries = append(entries, row)
		}
		return true
	})
	return entries
}

// Queue indications can be HTTP errors or JSON strings nested in SSE bodies.
func parseQoderQueueHint(payload []byte) (qoderQueueHint, bool) {
	var marker, hasRetry bool
	var hint qoderQueueHint
	var scan func(any, int)
	scan = func(value any, depth int) {
		if depth > 6 {
			return
		}
		switch v := value.(type) {
		case string:
			var next any
			if json.Unmarshal([]byte(v), &next) == nil {
				scan(next, depth+1)
			}
		case map[string]any:
			if seconds, ok := v["retryAfterSeconds"].(float64); ok && seconds >= 0 && seconds <= 86400 {
				hint.retry = time.Duration(seconds * float64(time.Second))
				hasRetry = true
			}
			if kind, ok := v["queueType"].(string); ok {
				hint.queueType = kind
			}
			available, hasAvailability := v["serviceAvailable"].(bool)
			queued, _ := v["isQueued"].(bool)
			marker = marker || queued || v["queueCount"] != nil || (hasAvailability && !available) || fmt.Sprint(v["code"]) == "10605"
			for _, key := range []string{"body", "message", "data", "error"} {
				if child, ok := v[key]; ok {
					scan(child, depth+1)
				}
			}
		}
	}
	var value any
	if json.Unmarshal(bytes.TrimSpace(payload), &value) != nil {
		return qoderQueueHint{}, false
	}
	scan(value, 0)
	return hint, marker && hasRetry
}

type queuePollFunc func(context.Context) (qoderQueueStatus, error)
type queueSleepFunc func(context.Context, time.Duration) error

func sleepQueue(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func clampQueueWait(value, minimum, maximum, remaining time.Duration) time.Duration {
	if value < minimum {
		value = minimum
	}
	if value > maximum {
		value = maximum
	}
	if value > remaining {
		value = remaining
	}
	if value < 0 {
		return 0
	}
	return value
}

func waitQoderQueue(ctx context.Context, deadline time.Time, hint qoderQueueHint, poll queuePollFunc, sleep queueSleepFunc, floor, ceiling time.Duration, touch func() error) (bool, error) {
	retry := hint.retry
	failures, noCapacity := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if touch != nil {
			if err := touch(); err != nil {
				return false, err
			}
		}
		status, err := poll(ctx)
		remaining := time.Until(deadline)
		if err != nil {
			failures++
			if failures >= 3 || remaining <= 0 {
				return false, nil
			}
			if err := sleep(ctx, clampQueueWait(time.Duration(failures)*floor, floor, ceiling, remaining)); err != nil {
				return false, err
			}
			continue
		}
		failures = 0
		if status.RetryAfterSeconds != nil && *status.RetryAfterSeconds >= 0 {
			retry = time.Duration(*status.RetryAfterSeconds) * time.Second
		}
		if !status.IsQueued {
			if err := sleep(ctx, clampQueueWait(retry, floor, ceiling, remaining)); err != nil {
				return false, err
			}
			return true, nil
		}
		if status.ServiceAvailable != nil && !*status.ServiceAvailable && status.QueueCount == 0 {
			noCapacity++
			if noCapacity >= 3 {
				return false, nil
			}
		} else {
			noCapacity = 0
		}
		if remaining <= 0 {
			return false, nil
		}
		if err := sleep(ctx, clampQueueWait(retry, floor, ceiling, remaining)); err != nil {
			return false, err
		}
	}
}

func queryQoderQueue(ctx context.Context, sa *storedAuth, requestSetID, requestID, model, queueType string) (qoderQueueStatus, error) {
	u, err := url.Parse(strings.TrimRight(sa.endpoints().Gateway, "/") + "/algo/api/v2/service/ask/queue/status")
	if err != nil {
		return qoderQueueStatus{}, err
	}
	q := u.Query()
	q.Set("requestSetId", requestSetID)
	q.Set("modelKey", model)
	if queueType != "" {
		q.Set("queueType", queueType)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return qoderQueueStatus{}, err
	}
	sess, err := cosySessionFor(sa)
	if err != nil {
		return qoderQueueStatus{}, err
	}
	headers, err := sess.headers(sa.Account.UID, "", u.String(), "application/json", false)
	if err != nil {
		return qoderQueueStatus{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Match the CLI identity used by the reference queue-status transport.
	emptyHash := md5.Sum(nil)
	req.Header.Set("Cosy-Machinetype", "5")
	req.Header.Set("Cosy-Machinetoken", sess.MachineID)
	req.Header.Set("Cosy-Machineos", buildMachineOS())
	req.Header.Set("Cosy-Clientip", "127.0.0.1")
	req.Header.Set("Cosy-Data-Policy", "disagree")
	req.Header.Set("Cosy-Bodyhash", fmt.Sprintf("%x", emptyHash))
	req.Header.Set("Cosy-Bodylength", "0")
	signaturePath, err := cosySigPath(u.String())
	if err != nil {
		return qoderQueueStatus{}, err
	}
	req.Header.Set("Cosy-Sigpath", signaturePath)
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return qoderQueueStatus{}, err
	}
	if resp.StatusCode != 200 {
		return qoderQueueStatus{}, fmt.Errorf("queue status HTTP %d", resp.StatusCode)
	}
	var status qoderQueueStatus
	if err = json.Unmarshal(resp.Body, &status); err != nil {
		return status, err
	}
	return status, nil
}

type replayQueueBody struct {
	io.Reader
	stream *hostHTTPStream
}

func (r *replayQueueBody) Close() error { r.stream.Close(); return nil }

// Peek only until the first data event. A request that has yielded content is
// never replayed; queue rejection before content may be polled and retried.
func inspectQueueStream(stream *hostHTTPStream, status int) (*hostHTTPStream, qoderQueueHint, bool, error) {
	reader := bufio.NewReader(newHostStreamReader(stream))
	var prefix bytes.Buffer
	if status >= 400 {
		data, err := io.ReadAll(io.LimitReader(reader, 1<<20))
		if err != nil {
			return stream, qoderQueueHint{}, false, err
		}
		prefix.Write(data)
		hint, ok := parseQoderQueueHint(data)
		if ok {
			return stream, hint, true, nil
		}
		return &hostHTTPStream{directBody: &replayQueueBody{Reader: bytes.NewReader(prefix.Bytes()), stream: stream}}, qoderQueueHint{}, false, nil
	}
	for prefix.Len() < 1<<20 {
		line, err := reader.ReadString('\n')
		prefix.WriteString(line)
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if hint, ok := parseQoderQueueHint([]byte(payload)); ok {
				return stream, hint, true, nil
			}
			return &hostHTTPStream{directBody: &replayQueueBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), stream: stream}}, qoderQueueHint{}, false, nil
		}
		if err != nil {
			if err != io.EOF {
				return stream, qoderQueueHint{}, false, err
			}
			break
		}
	}
	return &hostHTTPStream{directBody: &replayQueueBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), stream: stream}}, qoderQueueHint{}, false, nil
}

func openChatWithQueue(req *http.Request, sa *storedAuth, encoded, model string, touch func() error) (*hostHTTPStream, int, http.Header, error) {
	settings := currentQueueSettings()
	deadline := time.Now().Add(settings.budget)
	raw, err := qoderDecode([]byte(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		return nil, 0, nil, err
	}
	requestSetID, _ := body["request_set_id"].(string)
	publicModel := model
	if entry, ok := metadataForModel(sa, model); ok {
		publicModel = publicModelID(entry.DisplayName, entry.key())
	}
	defer queueProgress.Delete(requestSetID)
	for {
		stream, status, headers, err := openSignedChatStream(req, sa, encoded, model)
		if err != nil {
			return nil, status, headers, err
		}
		if !settings.enabled || requestSetID == "" {
			return stream, status, headers, nil
		}
		inspected, hint, queued, err := inspectQueueStream(stream, status)
		if err != nil {
			stream.Close()
			return nil, status, headers, err
		}
		if !queued {
			return inspected, status, headers, nil
		}
		stream.Close()
		queueProgress.Store(requestSetID, queueProgressEntry{Model: publicModel, Region: string(sa.region()), QueueType: hint.queueType, UpdatedAt: time.Now().Format(time.RFC3339)})
		waitCtx, cancel := context.WithDeadline(req.Context(), deadline)
		ready, err := waitQoderQueue(waitCtx, deadline, hint, func(ctx context.Context) (qoderQueueStatus, error) {
			requestID, _ := body["request_id"].(string)
			status, err := queryQoderQueue(ctx, sa, requestSetID, requestID, model, hint.queueType)
			if err == nil {
				queueProgress.Store(requestSetID, queueProgressEntry{Model: publicModel, Region: string(sa.region()), QueueType: hint.queueType, QueueCount: status.QueueCount, WaitSeconds: status.WaitTime, UpdatedAt: time.Now().Format(time.RFC3339)})
			}
			return status, err
		}, sleepQueue, 2*time.Second, 30*time.Second, touch)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && req.Context().Err() == nil {
				return nil, status, headers, fmt.Errorf("上游排队等待超时：model=%s retry_after=%ds", model, int(hint.retry.Seconds()))
			}
			return nil, status, headers, err
		}
		if !ready {
			return nil, status, headers, fmt.Errorf("上游排队等待超时或不可用：model=%s retry_after=%s", model, strconv.Itoa(int(hint.retry.Seconds()))+"s")
		}
		id := uuid.NewString()
		body["request_id"], body["chat_record_id"] = id, id
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, status, headers, err
		}
		encoded = qoderEncode(raw)
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(strings.NewReader(encoded))
		if err = applyCosyHeaders(req, sa, encoded, req.URL.String(), model, true); err != nil {
			return nil, status, headers, err
		}
	}
}
