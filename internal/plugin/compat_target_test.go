package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
)

// The observed failure: the plugin picked one of the key's own Codex accounts,
// the host's conductor moved the attempt onto the OpenAI-compatible provider in
// the same window, and the replayed history reached DeepSeek without the
// `reasoning_content` its thinking mode requires. The plugin only sees the pick,
// so a window that offers an API provider has to arm the repair for the models
// executed afterwards.
func TestPickCandidatesServeAPI(t *testing.T) {
	cases := []struct {
		name       string
		providers  []string
		candidates []service.AuthPickCandidate
		want       bool
	}{
		{
			name:      "providers list names the compat provider",
			providers: []string{"codex", "openai-compatible-deepseek"},
			want:      true,
		},
		{
			name: "candidate alone names the compat provider",
			candidates: []service.AuthPickCandidate{
				{ID: "codex-1", Provider: "codex"},
				{ID: "openai-compatibility:deepseek:1a96cef1", Provider: "openai-compatible-deepseek"},
			},
			want: true,
		},
		{
			name: "codex-only window stays unchanged",
			candidates: []service.AuthPickCandidate{
				{ID: "codex-1", Provider: "codex"},
				{ID: "codex-2", Provider: "codex"},
			},
			want: false,
		},
		{
			name:      "empty window",
			providers: nil,
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickCandidatesServeAPI(tc.providers, tc.candidates); got != tc.want {
				t.Fatalf("pickCandidatesServeAPI = %v, want %v", got, tc.want)
			}
		})
	}
}

// A recorded window arms the repair even when the plugin's own dispatcher served
// a Codex account, and the armed state is scoped to the model.
func TestCompatTargetsAPIProviderUsesRecordedWindow(t *testing.T) {
	resetCompatAPITargets()
	t.Cleanup(resetCompatAPITargets)

	codexRequest := func(model string) pluginapi.ExecutorRequest {
		return pluginapi.ExecutorRequest{
			Model:        model,
			AuthID:       "codex-43b33233-gdgpt3@163.com-prolite.json",
			AuthProvider: "codex",
		}
	}
	if compatTargetsAPIProvider(codexRequest("gpt-6-astra")) {
		t.Fatal("an unrecorded model must not be repaired")
	}
	recordCompatAPITarget("gpt-6-astra")
	if !compatTargetsAPIProvider(codexRequest("gpt-6-astra")) {
		t.Fatal("a recorded model must be repaired even on a codex attempt")
	}
	if compatTargetsAPIProvider(codexRequest("gpt-6.1-sol")) {
		t.Fatal("an unrelated model must not inherit the recorded window")
	}
}

// The repair itself on the shape DeepSeek rejected: an assistant turn that made a
// tool call and carries no reasoning of its own.
func TestRepairChatReasoningFillsToolTurns(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
		`{"role":"assistant","content":"done"}` +
		`],"max_tokens":8}`)
	rewritten, stripped := sanitizeCompatRequestBody(body)
	if len(stripped) == 0 || !strings.Contains(strings.Join(stripped, ","), "reasoning_content:injected") {
		t.Fatalf("repair not reported: %#v", stripped)
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatal(err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[1].(map[string]any)
	second, _ := messages[3].(map[string]any)
	if got, _ := first["reasoning_content"].(string); got != compatReasoningPlaceholder {
		t.Fatalf("tool-call assistant reasoning = %q, want the placeholder", got)
	}
	if got, _ := second["reasoning_content"].(string); got != compatReasoningPlaceholder {
		t.Fatalf("assistant after a tool result reasoning = %q, want the placeholder", got)
	}
}
