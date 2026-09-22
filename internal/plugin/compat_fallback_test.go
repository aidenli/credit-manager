package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// TestFallbackAttemptRewritesBodyAndPinsProvider covers what an API-provider
// fallback attempt must do to the nested execution:
//
//   - the pinned provider keeps the host's own selector from moving the request
//     to a provider that cannot serve the body;
//   - a structured-output declaration is downgraded before the provider rejects
//     it, because that rejection reached clients as a hard 500;
//   - the rewrite and the failure are written to the host log, which is the only
//     trace the plugin side has.
func TestFallbackAttemptRewritesBodyAndPinsProvider(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	key := mintFallbackTestKey(t, ctx, svc)

	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	var hostExecution pluginapi.HostModelExecutionRequest
	var forcedProvider string
	var logs []string
	var released string
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		var result any = map[string]any{}
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			var req struct {
				pluginapi.HostModelExecutionRequest
				ForcedProvider string `json:"forced_provider"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, 0, err
			}
			hostExecution = req.HostModelExecutionRequest
			forcedProvider = req.ForcedProvider
			return nil, 0, errors.New(`host_call_failed: {"error":{"message":"This response_format type is unavailable now"}}`)
		case pluginabi.MethodHostLog:
			var req struct {
				Message string         `json:"message"`
				Fields  map[string]any `json:"fields"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, 0, err
			}
			logs = append(logs, req.Message)
		case pluginabi.MethodHostModelStreamClose:
		default:
			return nil, 0, fmt.Errorf("unexpected host method %s", method)
		}
		encoded, err := okEnvelope(result)
		return encoded, 0, err
	}

	body := []byte(`{"model":"gpt-5.6-luna","stream":true,"input":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"planned"}]},{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"1"},{"type":"custom_tool_call_output","call_id":"c1","output":"ok"},{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"2"}],"text":{"format":{"type":"json_schema","name":"x","schema":{}}},"max_output_tokens":16}`)
	err := runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "gpt-5.6-luna", SourceFormat: "openai-response", Format: "openai-response", Stream: true,
		AuthID: "openai-compatibility:deepseek:1a96cef1d695", AuthProvider: "openai-compatible-deepseek",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}}, Payload: body,
	}}, "downstream", "")
	if err == nil {
		t.Fatal("failed nested execution must surface an error")
	}

	if forcedProvider != "openai-compatible-deepseek" {
		t.Fatalf("forced provider = %q", forcedProvider)
	}
	var sent map[string]any
	if err := json.Unmarshal(hostExecution.Body, &sent); err != nil {
		t.Fatalf("nested body is not JSON: %v (%s)", err, hostExecution.Body)
	}
	text, _ := sent["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format == nil || format["type"] != "json_object" {
		t.Fatalf("structured output was not downgraded for the API provider: %s", hostExecution.Body)
	}
	// The second tool call had no reasoning item of its own, so the provider's
	// thinking-mode contract needs the field supplied.
	items, _ := sent["input"].([]any)
	if len(items) != 4 {
		t.Fatalf("nested input items = %#v", sent["input"])
	}
	first, _ := items[1].(map[string]any)
	second, _ := items[3].(map[string]any)
	if reasoning, _ := first["reasoning_content"].(string); reasoning != "" {
		t.Fatalf("covered tool call gained a placeholder: %#v", first)
	}
	if reasoning, _ := second["reasoning_content"].(string); reasoning != compatReasoningPlaceholder {
		t.Fatalf("uncovered tool call reasoning = %#v", second)
	}
	if len(logs) < 2 {
		t.Fatalf("host log lines = %#v, want a rewrite and a failure line", logs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "text.format:json_schema->json_object") {
		t.Fatalf("rewrite was not logged: %#v", logs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "1x reasoning_content:injected") {
		t.Fatalf("reasoning repair was not logged: %#v", logs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "response_format type is unavailable") {
		t.Fatalf("failure text was not logged: %#v", logs)
	}
	entries, _, err := svc.Store().ListReleasedReservations(ctx, store.ReleasedReservationFilter{Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("released reservations = %#v, err = %v", entries, err)
	}
	released = entries[0].Reason
	if !strings.HasPrefix(released, "upstream_stream_error: ") || !strings.Contains(released, "response_format type is unavailable") {
		t.Fatalf("release reason lost the upstream text: %q", released)
	}
}

// TestBoundAccountAttemptKeepsStructuredOutputAndPinsCodex is the other half:
// when a bound OAuth account serves the request, the body must reach it
// unchanged, and the nested execution must stay on that provider.
func TestBoundAccountAttemptKeepsStructuredOutputAndPinsCodex(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	key := mintFallbackTestKey(t, ctx, svc)

	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	var sent []byte
	var forcedProvider string
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		var result any = map[string]any{}
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			var req struct {
				pluginapi.HostModelExecutionRequest
				ForcedProvider string `json:"forced_provider"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, 0, err
			}
			sent = req.Body
			forcedProvider = req.ForcedProvider
			result = pluginapi.HostModelStreamResponse{StreamID: "upstream", StatusCode: http.StatusOK}
		case pluginabi.MethodHostModelStreamRead:
			result = pluginapi.HostModelStreamReadResponse{Done: true}
		case pluginabi.MethodHostLog:
		case pluginabi.MethodHostStreamEmit:
		case pluginabi.MethodHostModelStreamClose:
		default:
			return nil, 0, fmt.Errorf("unexpected host method %s", method)
		}
		encoded, err := okEnvelope(result)
		return encoded, 0, err
	}

	body := []byte(`{"model":"gpt-5.6-luna","stream":true,"input":"hi","text":{"format":{"type":"json_schema","name":"x","schema":{}}},"max_output_tokens":16}`)
	if err := runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "gpt-5.6-luna", SourceFormat: "openai-response", Format: "openai-response", Stream: true,
		AuthID: "codex-43b33233-gdgpt3@163.com-prolite.json", AuthProvider: "codex",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}}, Payload: body,
	}}, "downstream", ""); err != nil {
		t.Fatal(err)
	}
	if forcedProvider != "codex" {
		t.Fatalf("forced provider = %q", forcedProvider)
	}
	var payload map[string]any
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("nested body is not JSON: %v", err)
	}
	text, _ := payload["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format == nil || format["type"] != "json_schema" {
		t.Fatalf("bound-account body was rewritten: %s", sent)
	}
}

func mintFallbackTestKey(t *testing.T, ctx context.Context, svc *service.Service) string {
	t.Helper()
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	_, material, err := svc.MintKey(ctx, service.BootstrapCallerID, "compat-fallback", 1_000_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	return material.Plaintext
}
