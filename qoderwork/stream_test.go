package main

import (
	"strings"
	"testing"
)

func TestUnwrapUpstreamChunk(t *testing.T) {
	cases := map[string]string{
		// Current agent envelope.
		`{"headers":{"Content-Type":["application/json"]},"body":"{\"choices\":[]}"}`: `{"choices":[]}`,
		// Already unwrapped.
		`{"choices":[{"delta":{"content":"x"}}]}`: `{"choices":[{"delta":{"content":"x"}}]}`,
		// Alternate nesting keys.
		`{"data":"{\"choices\":[]}"}`:    `{"choices":[]}`,
		`{"chunk":"{\"choices\":[]}"}`:   `{"choices":[]}`,
		`{"message":"{\"choices\":[]}"}`: `{"choices":[]}`,
		// Error frames are forwarded so the client sees them.
		`{"error":{"message":"boom"}}`: `{"error":{"message":"boom"}}`,
		// Alternate nesting keys used by other gateway builds.
		`{"payload":"{\"choices\":[]}"}`:                   `{"choices":[]}`,
		`{"event":"message","payload":"{\"choices\":[]}"}`: `{"choices":[]}`,
		// Double-encoded SSE (a JSON string holding the chunk object).
		`"{\"choices\":[]}"`: `{"choices":[]}`,
		// Nothing usable.
		`[DONE]`:  "",
		`{}`:      "",
		`garbage`: "",
		``:        "",
		`{"a":1}`: "",
	}
	for in, want := range cases {
		if got := unwrapUpstreamChunk(in); got != want {
			t.Errorf("unwrap(%q) = %q, want %q", in, got, want)
		}
	}
}

// A CALL_COUNT_LIMIT refusal arrives as HTTP 200 with an envelope-level
// statusCodeValue, so it must be turned into an error instead of being
// swallowed as an ordinary chunk (which leaves clients spinning).
func TestQoderUpstreamStatusErrorReportsQueue(t *testing.T) {
	frame := `{"headers":{"Content-Type":["application/json"],"Retry-After":["30"]},"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"modelKey\\\\\\\":\\\\\\\"dfmodel\\\\\\\",\\\\\\\"queueCount\\\\\\\":5158,\\\\\\\"queueType\\\\\\\":\\\\\\\"p3\\\\\\\",\\\\\\\"retryAfterSeconds\\\\\\\":30,\\\\\\\"waitTime\\\\\\\":116}\\\",\\\"subCode\\\":\\\"CALL_COUNT_LIMIT\\\"}\",\"subCode\":\"CALL_COUNT_LIMIT\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}`
	err := qoderUpstreamStatusError(frame)
	if err == nil {
		t.Fatal("want error for queued frame")
	}
	for _, want := range []string{"queued", "dfmodel", "5158", "116", "CALL_COUNT_LIMIT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestQoderUpstreamStatusErrorIgnoresChunks(t *testing.T) {
	for _, frame := range []string{
		`{"headers":{"Content-Type":["application/json"]},"body":"{\"choices\":[]}"}`,
		`{"choices":[{"delta":{"content":"x"}}]}`,
		`{"statusCodeValue":200,"statusCode":"OK","body":"{\"choices\":[]}"}`,
	} {
		if err := qoderUpstreamStatusError(frame); err != nil {
			t.Fatalf("unexpected error for %s: %v", frame, err)
		}
	}
}

func TestAggregateQoderSSEFailsOnQueueFrame(t *testing.T) {
	frame := `data:{"headers":{"Content-Type":["application/json"],"Retry-After":["30"]},"body":"{\"code\":\"403\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"dfmodel\\\",\\\"queueCount\\\":7}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}` + "\n\n"
	if _, err := aggregateQoderSSE(strings.NewReader(frame), "dfmodel"); err == nil || !strings.Contains(err.Error(), "queued") {
		t.Fatalf("aggregate error = %v, want queued error", err)
	}
}
