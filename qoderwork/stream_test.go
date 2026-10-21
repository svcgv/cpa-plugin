package main

import "testing"

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
