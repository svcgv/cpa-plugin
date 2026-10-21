package main

import (
	"encoding/json"
	"testing"
)

// The host marshals a real *http.Response (StatusCode/Header/Body). Parsing it
// as status_code/headers/body yielded StatusCode 0, which callers treat as a
// transport failure — the catalog was discarded even though the body was valid.
func TestDecodeHostHTTPResponseGoFieldNames(t *testing.T) {
	payload := map[string]any{
		"StatusCode": 200,
		"Header":     map[string][]string{"Content-Type": {"application/json"}},
		"Body":       []byte(`{"assistant":[{"key":"auto"}]}`),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	status, headers, body := decodeHostHTTPResultForTest(raw)
	if status != 200 {
		t.Fatalf("status = %d want 200", status)
	}
	if headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", headers)
	}
	if string(body) != `{"assistant":[{"key":"auto"}]}` {
		t.Fatalf("body = %q", body)
	}
}

func TestDecodeHostHTTPResponseLegacyFieldNames(t *testing.T) {
	raw := []byte(`{"status_code":201,"headers":{"X-A":["b"]},"body":"aGk="}`)
	status, headers, body := decodeHostHTTPResultForTest(raw)
	if status != 201 {
		t.Fatalf("status = %d want 201", status)
	}
	if headers.Get("X-A") != "b" {
		t.Fatalf("headers = %v", headers)
	}
	if string(body) != "hi" {
		t.Fatalf("body = %q", body)
	}
}
