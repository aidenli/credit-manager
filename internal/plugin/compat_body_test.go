package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// The provider rejection that produced a hard 500 for clients was
// `{"error":{"message":"This response_format type is unavailable now"}}` on a
// request that carried a structured-output declaration.
func TestSanitizeCompatRequestBody(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantKeys []string
		absent   []string
		stripped []string
	}{
		{
			name:     "chat json_schema downgrades to json mode",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`,
			wantKeys: []string{"response_format"},
			stripped: []string{"response_format:json_schema->json_object"},
		},
		{
			name:     "chat json_object is left alone",
			body:     `{"model":"m","messages":[],"response_format":{"type":"json_object"}}`,
			wantKeys: []string{"response_format"},
		},
		{
			name:     "unknown response_format type is dropped",
			body:     `{"model":"m","messages":[],"response_format":{"type":"grammar","grammar":"x"}}`,
			absent:   []string{"response_format"},
			stripped: []string{"response_format:removed"},
		},
		{
			name:     "responses text.format downgrades to json mode",
			body:     `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"x","schema":{}},"verbosity":"low"}}`,
			wantKeys: []string{"text"},
			stripped: []string{"text.format:json_schema->json_object"},
		},
		{
			name:     "responses text without format is untouched",
			body:     `{"model":"m","input":"hi","text":{"verbosity":"low"}}`,
			wantKeys: []string{"text"},
		},
		{
			name: "plain body is untouched",
			body: `{"model":"m","messages":[]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewritten, stripped := sanitizeCompatRequestBody([]byte(tc.body))
			if len(stripped) != len(tc.stripped) {
				t.Fatalf("stripped = %#v, want %#v", stripped, tc.stripped)
			}
			for i := range tc.stripped {
				if stripped[i] != tc.stripped[i] {
					t.Fatalf("stripped = %#v, want %#v", stripped, tc.stripped)
				}
			}
			if len(tc.stripped) == 0 && string(rewritten) != tc.body {
				t.Fatalf("body changed without a rewrite: %s", rewritten)
			}
			var payload map[string]any
			if err := json.Unmarshal(rewritten, &payload); err != nil {
				t.Fatalf("rewritten body is not JSON: %v (%s)", err, rewritten)
			}
			for _, key := range tc.wantKeys {
				if _, ok := payload[key]; !ok {
					t.Fatalf("rewritten body lost %q: %s", key, rewritten)
				}
			}
			for _, key := range tc.absent {
				if _, ok := payload[key]; ok {
					t.Fatalf("rewritten body kept %q: %s", key, rewritten)
				}
			}
			if len(tc.stripped) > 0 {
				format := payload["response_format"]
				if format == nil {
					if text, ok := payload["text"].(map[string]any); ok {
						format = text["format"]
					}
				}
				if fmt, ok := format.(map[string]any); ok && fmt["type"] != "json_object" {
					t.Fatalf("downgrade did not produce json_object: %#v", format)
				}
			}
		})
	}
}

func TestSanitizeCompatRequestBodyRejectsNonObject(t *testing.T) {
	for _, body := range []string{"", "not json", `[1,2]`} {
		rewritten, stripped := sanitizeCompatRequestBody([]byte(body))
		if string(rewritten) != body || len(stripped) != 0 {
			t.Fatalf("body %q was rewritten: %s %#v", body, rewritten, stripped)
		}
	}
}

// A Responses body must reach its provider byte for byte. The plugin's own
// provider for that shape is Codex, whose upstream validates every `input[]`
// item and rejects an injected field outright:
//
//	Unknown parameter: 'input[194].reasoning_content'. (code unknown_parameter)
//
// A Responses conversation already carries `reasoning` items, which the host
// translator turns into the provider's own reasoning field, so the placeholder is
// never needed here - only on the Chat Completions history a Codex conversation
// is replayed as.
func TestSanitizeCompatRequestBodyLeavesResponsesReasoningAlone(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"planned"}]},
		{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"1"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"ok"},
		{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"2"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
	]}`
	rewritten, stripped := sanitizeCompatRequestBody([]byte(body))
	if string(rewritten) != body {
		t.Fatalf("a Responses body was rewritten: %s", rewritten)
	}
	if len(stripped) != 0 {
		t.Fatalf("stripped = %#v, want no rewrite", stripped)
	}
	if strings.Contains(string(rewritten), "reasoning_content") {
		t.Fatalf("a Responses body gained reasoning_content: %s", rewritten)
	}
}

func TestSanitizeCompatRequestBodyRepairsChatReasoning(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"thinking out loud"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"},
		{"role":"assistant","content":"done"},
		{"role":"assistant","content":"and more","reasoning_content":"kept"}
	]}`
	rewritten, stripped := sanitizeCompatRequestBody([]byte(body))
	if len(stripped) != 1 || stripped[0] != "2x reasoning_content:injected" {
		t.Fatalf("stripped = %#v", stripped)
	}
	var payload struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	got := make([]string, 0, len(payload.Messages))
	for _, message := range payload.Messages {
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		reasoning, _ := message["reasoning_content"].(string)
		got = append(got, reasoning)
	}
	want := []string{
		// Before any tool call: untouched.
		"",
		"[reasoning unavailable]",
		"[reasoning unavailable]",
		"kept",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("assistant messages = %#v, want %#v", got, want)
		}
	}
}

func TestSanitizeCompatRequestBodyLeavesReasoningAlone(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","content":"plain"}]}`
	rewritten, stripped := sanitizeCompatRequestBody([]byte(body))
	if string(rewritten) != body || len(stripped) != 0 {
		t.Fatalf("a conversation without tool calls was rewritten: %s %#v", rewritten, stripped)
	}
}
