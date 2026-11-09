package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// roleSequence decodes messages into their role strings, in order.
func roleSequence(t *testing.T, payload []byte) []string {
	t.Helper()
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	roles := make([]string, 0, len(obj.Messages))
	for _, m := range obj.Messages {
		roles = append(roles, m.Role)
	}
	return roles
}

// 8.0.61+ keeps role=developer when translating Responses input into chat
// completions. CodeBuddy answers that shape with 400 code 11128, so the plugin
// has to fold developer back into system before forwarding.
func TestPrepareUpstreamBody_DeveloperBecomesSystem(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[
		{"role":"system","content":"base"},
		{"role":"developer","content":"mode: plan"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":"mode: default"}
	]}`)
	out := prepareUpstreamBody(in, nil, &storedAuth{}, "auto")
	got := roleSequence(t, out)
	want := []string{"system", "system", "user", "system"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if string(out) == string(in) {
		t.Fatal("payload with developer roles must be rewritten")
	}
}

// A leading turn with no system instruction gets a placeholder prepended;
// existing messages keep their order and are never duplicated.
func TestPrepareUpstreamBody_PrependsPlaceholderWithoutDuplicating(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[
		{"role":"user","content":"first user turn"},
		{"role":"assistant","content":"ok"}
	]}`)
	out := prepareUpstreamBody(in, nil, &storedAuth{}, "auto")

	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) != 3 {
		t.Fatalf("message count = %d, want 3 (one placeholder prepended)", len(obj.Messages))
	}
	if obj.Messages[0].Role != "system" {
		t.Fatalf("first role = %q, want system", obj.Messages[0].Role)
	}
	if obj.Messages[1].Content != "first user turn" {
		t.Fatalf("original order changed: %v", obj.Messages[1])
	}
	if got := roleSequence(t, out); !reflect.DeepEqual(got, []string{"system", "user", "assistant"}) {
		t.Fatalf("roles = %v", got)
	}
}

// A leading developer instruction is folded to system in place and therefore
// already satisfies the system-first rule: no placeholder, no reordering.
func TestPrepareUpstreamBody_LeadingDeveloperSatisfiesFirstSystem(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[
		{"role":"developer","content":"dev"},
		{"role":"user","content":"first user turn"},
		{"role":"system","content":"late system"}
	]}`)
	out := prepareUpstreamBody(in, nil, &storedAuth{}, "auto")
	if got := roleSequence(t, out); !reflect.DeepEqual(got, []string{"system", "user", "system"}) {
		t.Fatalf("roles = %v, want [system user system]", got)
	}
}

// A plain user-only conversation (no instructions at all) must still open with
// a system message for both regions.
func TestPrepareUpstreamBody_InjectsSystemForCN(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	for _, domain := range []string{"www.codebuddy.cn", "copilot.tencent.com", "www.workbuddy.ai"} {
		sa := &storedAuth{Auth: storedTokens{Domain: domain}}
		out := prepareUpstreamBody(in, nil, sa, "auto")
		roles := roleSequence(t, out)
		if len(roles) == 0 || roles[0] != "system" {
			t.Fatalf("%s: roles = %v, want leading system", domain, roles)
		}
		if len(roles) != 2 {
			t.Fatalf("%s: roles = %v, want exactly one injected system", domain, roles)
		}
	}
}

// Already-valid conversations stay byte-identical apart from the forced
// stream flag, so the fix does not churn normal traffic.
func TestPrepareUpstreamBody_ValidConversationUntouched(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[
		{"role":"system","content":"base"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"bye"}
	],"stream":true}`)
	out := prepareUpstreamBody(in, nil, &storedAuth{}, "auto")
	roles := roleSequence(t, out)
	want := []string{"system", "user", "assistant", "user"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
}

// Unknown/garbage roles must not reach the upstream verbatim.
func TestNormalizeMessageRolesInPlace_UnknownRoleBecomesSystem(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "SYSTEM", "content": "base"},
		map[string]any{"role": "critic", "content": "weird"},
		map[string]any{"role": "User", "content": "hi"},
	}}
	if !normalizeMessageRolesInPlace(obj) {
		t.Fatal("expected modification")
	}
	messages := obj["messages"].([]any)
	got := make([]string, 0, len(messages))
	for _, m := range messages {
		got = append(got, m.(map[string]any)["role"].(string))
	}
	if !reflect.DeepEqual(got, []string{"system", "system", "user"}) {
		t.Fatalf("roles = %v", got)
	}
}
