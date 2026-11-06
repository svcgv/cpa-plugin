package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This opt-in integration test reads a local PAT without printing it or writing
// credentials. The optional check-in changes only the tested account's reward.
func TestLivePATIntegration(t *testing.T) {
	path := os.Getenv("QODER_TEST_PAT_PATH")
	if path == "" {
		t.Skip("set QODER_TEST_PAT_PATH to run the live integration")
	}
	if rawSeconds := os.Getenv("QODER_TEST_QUEUE_SECONDS"); rawSeconds != "" {
		seconds, err := strconv.Atoi(rawSeconds)
		if err != nil || seconds < 1 {
			t.Fatal("invalid test queue budget")
		}
		previous := currentQueueSettings()
		setQueueSettings(true, seconds)
		defer setQueueSettings(previous.enabled, int(previous.budget/time.Second))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read PAT file")
	}
	pat := strings.TrimSpace(string(raw))
	region := parseRegionValue(os.Getenv("QODER_TEST_REGION"))
	tok, err := exchangePATForJobTokenAt(endpointsForRegion(region).OpenAPI, pat)
	if err != nil {
		t.Fatal("PAT exchange failed")
	}
	ui, err := fetchUserInfoAt(endpointsForRegion(region).OpenAPI, tok.Token)
	if err != nil || ui == nil || ui.ID == "" {
		t.Fatal("userinfo validation failed")
	}
	sa := buildStoredAuthFromJobToken(pat, tok, ui)
	sa.Auth.Region, sa.Auth.Domain = string(region), domainForRegion(region)
	t.Log("PAT exchange and account identity verified")
	models, err := callModelsForAuth(sa)
	if err != nil || len(models) == 0 {
		t.Fatalf("model discovery failed: %v", err)
	}
	t.Logf("discovered %d models", len(models))
	rows, err := fetchCampaigns(context.Background(), sa)
	if err != nil {
		t.Fatalf("campaign query failed: %v", err)
	}
	t.Logf("campaign query succeeded: %d campaigns", len(rows))
	if os.Getenv("QODER_TEST_CHECKIN") == "1" {
		result, err := claimCampaigns(context.Background(), sa, rows)
		if err != nil || result["success"] != true {
			t.Fatalf("check-in failed: %v %v", err, result["message"])
		}
		t.Logf("check-in result: %v", result["message"])
	}
	model := models[0].ID
	requested := os.Getenv("QODER_TEST_MODEL")
	found := requested == ""
	for _, candidate := range models {
		if candidate.ID == requested || (requested == "" && candidate.ID == "auto") {
			model = candidate.ID
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("requested test model %s not found in catalog", requested)
	}
	request := map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": "Reply with the single word OK."}}, "stream": false}
	if effort := os.Getenv("QODER_TEST_REASONING"); effort != "" {
		request["reasoning_effort"] = effort
	}
	payload, _ := json.Marshal(request)
	storage, _ := json.Marshal(sa)
	req, _ := json.Marshal(pluginapi.ExecutorRequest{Model: model, Payload: payload, StorageJSON: storage})
	response, err := handleExecExecute(req)
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if len(response) == 0 {
		t.Fatal("empty completion")
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Payload []byte `json:"payload"`
		} `json:"result"`
	}
	if json.Unmarshal(response, &env) != nil || !env.OK {
		t.Fatal("completion envelope invalid")
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(env.Result.Payload, &completion) != nil || len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		t.Fatal("completion contains no assistant text")
	}
	t.Logf("chat completion succeeded with model %s", model)
	if os.Getenv("QODER_TEST_STREAM") == "1" {
		streamReq, _ := json.Marshal(executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: model, Payload: payload, StorageJSON: storage}})
		rawStream, err := handleExecStream(streamReq)
		if err != nil {
			t.Fatalf("stream request failed: %v", err)
		}
		var streamResult struct {
			Result struct {
				Chunks []pluginapi.ExecutorStreamChunk `json:"chunks"`
			} `json:"result"`
		}
		if json.Unmarshal(rawStream, &streamResult) != nil || len(streamResult.Result.Chunks) == 0 {
			t.Fatal("stream returned no chunks")
		}
		t.Logf("stream returned %d chunks", len(streamResult.Result.Chunks))
	}
	if os.Getenv("QODER_TEST_TOOLS") == "1" {
		toolPayload := map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": "Use the lookup_test tool to look up the word apple. Do not answer without calling it."}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup_test", "description": "Look up a word", "parameters": map[string]any{"type": "object", "properties": map[string]any{"word": map[string]any{"type": "string"}}, "required": []string{"word"}}}}}, "tool_choice": "required"}
		rawTool, _ := json.Marshal(toolPayload)
		wire, _ := json.Marshal(pluginapi.ExecutorRequest{Model: model, Payload: rawTool, StorageJSON: storage})
		result, err := handleExecExecute(wire)
		if err != nil {
			t.Fatalf("tool request failed: %v", err)
		}
		var envelope struct {
			Result struct {
				Payload []byte `json:"payload"`
			} `json:"result"`
		}
		var answer struct {
			Choices []struct {
				Message struct {
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(result, &envelope) != nil || json.Unmarshal(envelope.Result.Payload, &answer) != nil || len(answer.Choices) == 0 || len(answer.Choices[0].Message.ToolCalls) == 0 {
			t.Fatal("model did not return a tool call")
		}
		t.Log("tool-call request returned a tool call")
		var call struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(answer.Choices[0].Message.ToolCalls[0], &call) != nil || call.ID == "" {
			t.Fatal("tool call has no id")
		}
		toolPayload["tool_choice"] = "auto"
		toolPayload["messages"] = []any{
			map[string]any{"role": "user", "content": "Use the lookup_test tool to look up the word apple. Then tell me its meaning."},
			map[string]any{"role": "assistant", "content": "", "tool_calls": answer.Choices[0].Message.ToolCalls},
			map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "apple means a fruit"},
		}
		continued, _ := json.Marshal(toolPayload)
		wire, _ = json.Marshal(pluginapi.ExecutorRequest{Model: model, Payload: continued, StorageJSON: storage})
		result, err = handleExecExecute(wire)
		if err != nil {
			t.Fatalf("tool result continuation failed: %v", err)
		}
		if json.Unmarshal(result, &envelope) != nil || json.Unmarshal(envelope.Result.Payload, &completion) != nil || len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
			t.Fatal("tool result continuation returned no assistant text")
		}
		t.Log("tool-result continuation returned assistant text")
	}
}
