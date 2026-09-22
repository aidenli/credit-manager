package plugin

import (
	"encoding/json"
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
