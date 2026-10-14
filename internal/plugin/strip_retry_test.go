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
)

// The failure this covers, seen in production: turn A was answered by one Codex
// account, turn B resumed with that account's `encrypted_content`, and the host
// dispatched B to a different account of the same model. The upstream could not
// hydrate the blob, so every retry replayed the same poisoned items and the
// conversation could never continue.
//
// The plugin cannot re-sign the blob and cannot choose the account the host's
// conductor finally uses, so it removes the account-bound state and retries once:
// the client still gets an answer, on the same model, from the same request.
func TestStreamRetriesWithoutEncryptedReasoningAfterSignatureFailure(t *testing.T) {
	ctx := context.Background()
	resetCompatAPITargets()
	t.Cleanup(resetCompatAPITargets)
	svc := newLifecycleTestService(t)
	key := mintFallbackTestKey(t, ctx, svc)

	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	var bodies [][]byte
	emitted := 0
	closeCount := 0
	reads := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		var result any = map[string]any{}
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			var req struct {
				pluginapi.HostModelExecutionRequest
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, 0, err
			}
			bodies = append(bodies, append([]byte(nil), req.Body...))
			result = pluginapi.HostModelStreamResponse{StreamID: "upstream", StatusCode: http.StatusOK}
		case pluginabi.MethodHostModelStreamRead:
			reads++
			switch reads {
			case 1:
				// In-stream terminal failure, before any payload reached the client.
				result = pluginapi.HostModelStreamReadResponse{Error: `{"error":{"message":"The encrypted content for item amsg_01a1195a-b414-7512-906b-e6d57f87e2ca could not be verified. Reason: encrypted content hydration failed: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","code":"thinking_signature_invalid"}}`}
			case 2:
				result = pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {\"type\":\"response.completed\"}")}
			default:
				result = pluginapi.HostModelStreamReadResponse{Done: true}
			}
		case pluginabi.MethodHostStreamEmit:
			emitted++
		case pluginabi.MethodHostModelStreamClose:
			closeCount++
		case pluginabi.MethodHostLog:
		default:
			return nil, 0, fmt.Errorf("unexpected host method %s", method)
		}
		encoded, err := okEnvelope(result)
		return encoded, 0, err
	}

	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","id":"rs_a","encrypted_content":"gAAAA-account-a","summary":[{"type":"summary_text","text":"planned"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"max_output_tokens":16}`)
	keyHeaders := http.Header{"Authorization": []string{"Bearer " + key}}
	err := runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "gpt-6-astra", SourceFormat: "openai-response", Format: "openai-response", Stream: true,
		AuthID: "codex-43b33233-gdgpt3@163.com-prolite.json", AuthProvider: "codex",
		Headers: keyHeaders, Payload: body,
	}}, "downstream", "")
	if err != nil {
		t.Fatalf("the stripped retry did not rescue the request: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (rejected replay plus stripped retry)", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), "encrypted_content") {
		t.Fatalf("first attempt was already stripped: %s", bodies[0])
	}
	if strings.Contains(string(bodies[1]), "encrypted_content") {
		t.Fatalf("retry still carried account-bound reasoning: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), `"rs_a"`) {
		t.Fatalf("retry still carried the account-bound item id: %s", bodies[1])
	}
	if !strings.Contains(string(bodies[1]), "planned") {
		t.Fatalf("retry dropped the readable summary: %s", bodies[1])
	}
	if emitted == 0 {
		t.Fatal("the successful retry was not forwarded downstream")
	}
	if closeCount < 2 {
		t.Fatalf("upstream streams closed = %d, want one per attempt", closeCount)
	}
	// The credential and model are remembered, so the next turn is stripped before
	// the upstream ever rejects it.
	if !compatReasoningPoisoned(compatRequestKey(keyHeaders), "gpt-6-astra") {
		t.Fatal("the failure was not remembered for the next request")
	}
}

// Both request shapes lose the account-bound state but keep what the model can
// still read. A body with nothing account-bound reports zero, which is what stops
// the retry from looping.
func TestStripCompatEncryptedReasoning(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		keep []string
		gone []string
	}{
		{
			name: "responses reasoning item",
			body: `{"model":"m","input":[{"type":"reasoning","id":"rs_a","encrypted_content":"gAAAA-account-a","summary":[{"type":"summary_text","text":"planned"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`,
			want: 1,
			keep: []string{"planned", "done"},
			gone: []string{"encrypted_content", "gAAAA-account-a"},
		},
		{
			name: "chat history with reasoning_content",
			body: `{"model":"m","messages":[{"role":"assistant","content":"a","reasoning_content":"secret chain"},{"role":"assistant","content":"b","reasoning_content":""}],"max_tokens":8}`,
			want: 2,
			keep: []string{"\"reasoning_content\":\"\""},
		},
		{
			// "Encrypted function output content could not be decrypted or
			// decoded." - the blob rides on the tool-result item as a sibling of
			// output. The call_id pairing and readable output survive; the item
			// id (also account-minted) goes with the blob.
			name: "function_call_output with sibling encrypted_content",
			body: `{"model":"m","input":[{"type":"function_call_output","call_id":"call_1","id":"fco_a","encrypted_content":"gAAAA-fn-a","output":"result text"}]}`,
			want: 1,
			keep: []string{"result text", "call_1"},
			gone: []string{"encrypted_content", "gAAAA-fn-a", "fco_a"},
		},
		{
			// Structured tool output: the encryption rides inside an output
			// content part. Only the encrypted field is removed; the readable
			// part text and the call_id pairing survive.
			name: "custom_tool_call_output with nested encrypted part",
			body: `{"model":"m","input":[{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"visible","encrypted_content":"gAAAA-fn-b"}]}]}`,
			want: 1,
			keep: []string{"visible", "call_2"},
			gone: []string{"encrypted_content", "gAAAA-fn-b"},
		},
		{
			name: "nothing account-bound",
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want: 0,
		},
		{
			name: "not JSON",
			body: `not json`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewritten, got := stripCompatEncryptedReasoning([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("stripped = %d, want %d (%s)", got, tc.want, rewritten)
			}
			for _, want := range tc.keep {
				if !strings.Contains(string(rewritten), want) {
					t.Fatalf("rewrite dropped %q: %s", want, rewritten)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(string(rewritten), gone) {
					t.Fatalf("rewrite kept %q: %s", gone, rewritten)
				}
			}
		})
	}
}

// A remembered failure strips the next request before the upstream sees it.
func TestPrepareCompatBodyStripsAfterMarkedFailure(t *testing.T) {
	resetCompatAPITargets()
	t.Cleanup(resetCompatAPITargets)

	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"reasoning","id":"rs_a","encrypted_content":"gAAAA-account-a","summary":[{"type":"summary_text","text":"planned"}]}],"max_output_tokens":16}`)
	headers := http.Header{"Authorization": []string{"Bearer tk-test"}}
	request := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "gpt-6-astra", AuthProvider: "codex", Headers: headers, Payload: body,
	}}

	// Without a recorded window and without a failure mark, the replay is untouched.
	if rewritten := prepareCompatBody(request, body); !strings.Contains(string(rewritten), "encrypted_content") {
		t.Fatalf("body rewritten before any failure was seen: %s", rewritten)
	}
	markCompatPoisonedReasoning(compatRequestKey(headers), "gpt-6-astra")
	rewritten := prepareCompatBody(request, body)
	if strings.Contains(string(rewritten), "encrypted_content") {
		t.Fatalf("remembered failure did not strip the replay: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), "planned") {
		t.Fatalf("strip dropped the readable summary: %s", rewritten)
	}
}

// A codex attempt keeps its encrypted state even when the model's scheduling
// window offered an API provider: the account itself can decrypt it, and a
// compaction item without its required encrypted_content is rejected upstream
// ("Missing required parameter: 'input[50].encrypted_content'").
func TestPrepareCompatBodyKeepsEncryptedStateForCodexWithAPIWindow(t *testing.T) {
	resetCompatAPITargets()
	t.Cleanup(resetCompatAPITargets)
	recordCompatAPITarget("gpt-6-astra")

	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"reasoning","id":"rs_a","encrypted_content":"gAAAA-account-a","summary":[]},{"type":"compaction","encrypted_content":"gAAAA-cmp-a"}],"max_output_tokens":16}`)
	headers := http.Header{"Authorization": []string{"Bearer tk-fresh"}}
	request := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "gpt-6-astra", AuthProvider: "codex", Headers: headers, Payload: body,
	}}

	rewritten := string(prepareCompatBody(request, body))
	if !strings.Contains(rewritten, "gAAAA-account-a") || !strings.Contains(rewritten, "gAAAA-cmp-a") {
		t.Fatalf("codex replay lost encrypted state before send: %s", rewritten)
	}
}

// The reasoning strip never leaves a compaction item without its required
// encrypted_content; dropCompactionItems removes the item whole.
func TestCompactionItemsStayValid(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"compaction","id":"cmp_1","encrypted_content":"gAAAA-cmp"},{"type":"reasoning","encrypted_content":"gAAAA-rs","summary":[]},{"type":"message","role":"user","content":"hi"}]}`)

	stripped, count := stripCompatEncryptedReasoning(body)
	if count != 1 || strings.Contains(string(stripped), "gAAAA-rs") {
		t.Fatalf("reasoning not stripped alone: %d %s", count, stripped)
	}
	if !strings.Contains(string(stripped), `"encrypted_content":"gAAAA-cmp"`) || !strings.Contains(string(stripped), "cmp_1") {
		t.Fatalf("compaction item was modified: %s", stripped)
	}

	dropped, n := dropCompactionItems(stripped)
	if n != 1 || strings.Contains(string(dropped), "compaction") || !strings.Contains(string(dropped), "hi") {
		t.Fatalf("compaction not dropped whole: %d %s", n, dropped)
	}
	if _, n := dropCompactionItems(dropped); n != 0 {
		t.Fatalf("second drop removed %d items", n)
	}

	fallback := string(apiFallbackBody(body, "deepseek-flash"))
	if strings.Contains(fallback, "compaction") || strings.Contains(fallback, "gAAAA") {
		t.Fatalf("API fallback body kept account-bound state: %s", fallback)
	}
}

func TestIsEncryptedReasoningFailureMatchesInvalidEncryptedContent(t *testing.T) {
	if !isEncryptedReasoningFailure(`{"error":{"code":"invalid_encrypted_content"}}`) {
		t.Fatal("invalid_encrypted_content not recognized")
	}
	if isEncryptedReasoningFailure("Missing required parameter: 'input[50].encrypted_content'.") {
		t.Fatal("missing_required_parameter must not trigger the strip retry")
	}
}
