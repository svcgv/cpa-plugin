package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestRealPayload(t *testing.T) {
	raw, _ := os.ReadFile("/tmp/harness_payload.json")
	out := prepareUpstreamBody(raw, nil, &storedAuth{}, "deepseek-v4.1-flash")
	t.Logf("in=%d out=%d changed=%v", len(raw), len(out), !bytes.Equal(raw, out))
	var src, dst map[string]any
	json.Unmarshal(raw, &src)
	json.Unmarshal(out, &dst)
	a := src["messages"].([]any)[0].(map[string]any)["content"].(string)
	b := dst["messages"].([]any)[0].(map[string]any)["content"].(string)
	t.Logf("src head=%q", a[:60])
	t.Logf("dst head=%q", b[:60])
	t.Logf("model %v -> %v", src["model"], dst["model"])
}
