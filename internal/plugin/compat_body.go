package plugin

import (
	"encoding/json"
	"strings"
)

// Structured-output fields an OpenAI-compatible API provider may reject.
//
// The reported failure was a bound key whose request the API provider answered
// with `{"error":{"message":"This response_format type is unavailable now"}}`,
// which the client saw as a hard 500. The request body carried a structured
// output request that the provider's API does not implement: Codex Desktop sends
// the Responses shape (`text.format`) and Chat Completions clients send
// `response_format`.
//
// JSON mode is the subset those providers do support, so a schema request is
// downgraded to it instead of being dropped: the client still receives parseable
// JSON, and the operator sees the rewrite in the host log.

type compatRewrite struct {
	fields []string
}

func (r *compatRewrite) add(field string) {
	if strings.TrimSpace(field) != "" {
		r.fields = append(r.fields, field)
	}
}

func (r *compatRewrite) empty() bool { return len(r.fields) == 0 }

// sanitizeCompatRequestBody rewrites the structured-output fields of a request
// body that an OpenAI-compatible API provider will serve. It returns the body
// unchanged (and no rewrites) when the body is not a JSON object or carries
// nothing to rewrite.
func sanitizeCompatRequestBody(body []byte) ([]byte, []string) {
	if len(body) == 0 {
		return body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body, nil
	}
	rewrite := &compatRewrite{}
	if format, ok := payload["response_format"].(map[string]any); ok {
		if replacement, changed := compatStructuredOutputFormat(format); changed {
			if replacement == nil {
				delete(payload, "response_format")
				rewrite.add("response_format:removed")
			} else {
				payload["response_format"] = replacement
				rewrite.add("response_format:json_schema->json_object")
			}
		}
	}
	if text, ok := payload["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if replacement, changed := compatStructuredOutputFormat(format); changed {
				if replacement == nil {
					delete(text, "format")
					rewrite.add("text.format:removed")
				} else {
					text["format"] = replacement
					rewrite.add("text.format:json_schema->json_object")
				}
				if len(text) == 0 {
					delete(payload, "text")
				} else {
					payload["text"] = text
				}
			}
		}
	}
	if rewrite.empty() {
		return body, nil
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}
	return rewritten, rewrite.fields
}

// compatStructuredOutputFormat maps one structured-output format declaration.
// A nil replacement means "drop the field".
func compatStructuredOutputFormat(format map[string]any) (map[string]any, bool) {
	kind := strings.ToLower(strings.TrimSpace(stringField(format, "type")))
	switch kind {
	case "", "text", "json_object":
		// Nothing to do: providers accept a plain text or JSON-mode request.
		return format, false
	case "json_schema":
		// Downgrade to JSON mode, keeping the request parseable for the client.
		return map[string]any{"type": "json_object"}, true
	default:
		return nil, true
	}
}

func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key].(string)
	if !ok {
		return ""
	}
	return value
}

// servesAPIProvider reports whether the credential the host dispatched this
// request to belongs to an OpenAI-compatible API provider, i.e. the bound key's
// fallback rather than one of its own OAuth accounts.
func servesAPIProvider(req executorAuthContext) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.provider)), "openai-compatible")
}

// executorAuthContext carries what the host told the plugin about the credential
// it dispatched the request to.
type executorAuthContext struct {
	authID   string
	provider string
}
