package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/store"
)

// A codex account answers with response.created and then an in-stream
// server_is_overloaded event. The preamble is held back, so the plugin can replay
// the request on the API provider with the configured model, for a model the
// provider does not alias.
func TestStreamFallsBackToAPIProviderOnOverload(t *testing.T) {
	ctx := context.Background()
	resetCompatAPITargets()
	t.Cleanup(resetCompatAPITargets)
	svc := newLifecycleTestService(t)
	key := mintFallbackTestKey(t, ctx, svc)
	if _, err := svc.UpdateAuthFallbackSettings(ctx, store.AuthFallbackSettings{Enabled: true}); err != nil {
		t.Fatal(err)
	}

	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	type sent struct {
		forced string
		model  string
		body   string
	}
	var attempts []sent
	var emitted []string
	reads := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		var result any = map[string]any{}
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			var req hostModelExecutionRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, 0, err
			}
			attempts = append(attempts, sent{req.ForcedProvider, req.Model, string(req.Body)})
			result = pluginapi.HostModelStreamResponse{StreamID: fmt.Sprintf("up%d", len(attempts)), StatusCode: http.StatusOK}
		case pluginabi.MethodHostModelStreamRead:
			reads++
			switch {
			case len(attempts) == 1 && reads == 1:
				result = pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[],"error":null}}`)}
			case len(attempts) == 1:
				result = pluginapi.HostModelStreamReadResponse{Error: `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`}
			case reads%2 == 1:
				result = pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"type":"response.completed"}`)}
			default:
				result = pluginapi.HostModelStreamReadResponse{Done: true}
			}
		case pluginabi.MethodHostStreamEmit:
			var req rpcStreamEmitRequest
			_ = json.Unmarshal(raw, &req)
			emitted = append(emitted, string(req.Payload))
		case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostLog:
		default:
			return nil, 0, fmt.Errorf("unexpected host method %s", method)
		}
		encoded, err := okEnvelope(result)
		return encoded, 0, err
	}

	body := []byte(`{"model":"gpt-6.1-sol","stream":true,"input":[{"type":"reasoning","id":"rs_a","encrypted_content":"gAAAA","summary":[]}],"max_output_tokens":16}`)
	err := runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "gpt-6.1-sol", SourceFormat: "openai-response", Format: "openai-response", Stream: true,
		AuthID: "codex-a.json", AuthProvider: "codex",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}}, Payload: body,
	}}, "downstream", "")
	if err != nil {
		t.Fatalf("fallback did not rescue the request: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if attempts[0].forced != "codex" {
		t.Fatalf("first attempt forced %q", attempts[0].forced)
	}
	second := attempts[1]
	if second.forced != "openai-compatible-deepseek" || second.model != "deepseek-flash" {
		t.Fatalf("fallback attempt = %+v", second)
	}
	if strings.Contains(second.body, "encrypted_content") || !strings.Contains(second.body, `"model":"deepseek-flash"`) {
		t.Fatalf("fallback body not prepared: %s", second.body)
	}
	for _, chunk := range emitted {
		if strings.Contains(chunk, "overloaded") {
			t.Fatalf("failure leaked to the client: %v", emitted)
		}
	}
	if len(emitted) == 0 {
		t.Fatal("nothing emitted")
	}
}

// The real codex response.created envelope embeds "error":null and an empty
// "output":[]; a substring test for "error" misreads it as content and emits it,
// defeating the retry. isStreamPreamble must classify by event type instead.
func TestStreamPreambleClassification(t *testing.T) {
	preamble := [][]byte{
		[]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[],"error":null}}`),
		[]byte(`data: {"type":"response.in_progress","response":{"id":"resp_1","error":null}}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"id":"rs","type":"reasoning","summary":[]},"output_index":0}`),
		[]byte(`data: {"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`),
		[]byte(":keepalive"),
		[]byte(""),
		[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"error\":null,\"output\":[]}}"),
	}
	for _, p := range preamble {
		if !isStreamPreamble(p) {
			t.Errorf("expected preamble, got content: %s", p)
		}
	}
	content := [][]byte{
		[]byte(`data: {"type":"response.output_text.delta","delta":"hi"}`),
		[]byte(`data: {"type":"response.completed","response":{"status":"completed"}}`),
		[]byte(`data: {"type":"response.failed","response":{"error":{"code":"thinking_signature_invalid"}}}`),
		[]byte(`data: {"type":"response.output_item.added","item":{"id":"rs","type":"reasoning","encrypted_content":"gAAAA","summary":[]}}`),
		[]byte(`data: {"type":"response.output_item.done","item":{"id":"m","type":"message","content":[{"type":"output_text","text":"x"}]}}`),
	}
	for _, p := range content {
		if isStreamPreamble(p) {
			t.Errorf("expected content, got preamble: %s", p)
		}
	}
}

func TestFallbackWorthyFailure(t *testing.T) {
	for _, text := range []string{
		`{"error":{"code":"server_is_overloaded"}}`,
		`{"error":{"type":"usage_limit_reached"}}`,
		"upstream stream status 429",
		"upstream status 502",
		"thinking_signature_invalid",
		`host_call_failed: Post "https://chatgpt.com/backend-api/codex/responses": utls: dial upstream: socks connect tcp 104.202.107.214:6023->chatgpt.com:443: dial tcp 104.202.107.214:6023: connect: connection refused`,
	} {
		if !isFallbackWorthyFailure(text) {
			t.Errorf("%q should fall back", text)
		}
	}
	for _, text := range []string{"", `{"error":{"code":"invalid_request_error","message":"bad field"}}`} {
		if isFallbackWorthyFailure(text) {
			t.Errorf("%q should not fall back", text)
		}
	}
	if isFallbackWorthyStatus(400) || !isFallbackWorthyStatus(429) || !isFallbackWorthyStatus(503) {
		t.Error("status classification wrong")
	}
}
