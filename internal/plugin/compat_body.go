package plugin

import (
	"encoding/json"
	"strconv"
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
//
// The second rejection of the same family is DeepSeek's thinking mode:
//
//	The `reasoning_content` in the thinking mode must be passed back to the API.
//
// Once a thinking-mode model has made a tool call, every assistant message in
// the replayed history must carry `reasoning_content`. A conversation that was
// produced by another provider (Codex here) has no such field on most assistant
// turns, so the translated request is rejected outright. The host translator
// substitutes a placeholder when it sees a reasoning item it cannot read, so a
// missing field means there was no reasoning item at all. Repairing that means
// adding the field, not stripping it.
type compatRewrite struct {
	fields []string
}

func (r *compatRewrite) add(field string) {
	if strings.TrimSpace(field) != "" {
		r.fields = append(r.fields, field)
	}
}

func (r *compatRewrite) addCount(field string, count int) {
	if count <= 0 {
		return
	}
	r.add(strconv.Itoa(count) + "x " + field)
}

func (r *compatRewrite) empty() bool { return len(r.fields) == 0 }

// compatReasoningPlaceholder mirrors the host translator's own substitute for a
// reasoning item it cannot read. The provider only needs the field present.
const compatReasoningPlaceholder = "[reasoning unavailable]"

// maxCompatReasoningRepairs bounds the work one body can cause.
const maxCompatReasoningRepairs = 2000

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
	rewrite.addCount("reasoning_content:injected", repairCompatReasoning(payload))
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

// hasReasoningContent reports whether an item already carries reasoning text, in
// which case it is never overwritten.
func hasReasoningContent(item map[string]any) bool {
	return strings.TrimSpace(stringField(item, "reasoning_content")) != ""
}

// repairCompatReasoning makes an API-provider request satisfy the thinking-mode
// contract: every assistant-side item that has no reasoning of its own and no
// preceding reasoning item receives the placeholder the host translator itself
// uses. Returns how many items were repaired.
func repairCompatReasoning(payload map[string]any) int {
	if input, ok := payload["input"].([]any); ok {
		return repairResponsesReasoning(input)
	}
	if messages, ok := payload["messages"].([]any); ok {
		return repairChatReasoning(messages)
	}
	return 0
}

// repairResponsesReasoning walks a Responses payload's input items. Any
// `reasoning` item supplies the reasoning for the assistant-side items that
// follow it, mirroring how the host translator attaches it.
func repairResponsesReasoning(items []any) int {
	pending := false
	repaired := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringField(item, "type")))
		if kind == "" && stringField(item, "role") != "" {
			kind = "message"
		}
		switch kind {
		case "reasoning":
			pending = true
		case "message":
			if !strings.EqualFold(strings.TrimSpace(stringField(item, "role")), "assistant") {
				continue
			}
			if !pending && !hasReasoningContent(item) && repaired < maxCompatReasoningRepairs {
				item["reasoning_content"] = compatReasoningPlaceholder
				repaired++
			}
			pending = false
		case "function_call", "custom_tool_call":
			if !pending && !hasReasoningContent(item) && repaired < maxCompatReasoningRepairs {
				item["reasoning_content"] = compatReasoningPlaceholder
				repaired++
			}
			pending = false
		}
	}
	return repaired
}

// repairChatReasoning walks a Chat Completions payload. The provider only
// requires reasoning once a tool call has happened, so plain assistant turns
// before that are left untouched.
func repairChatReasoning(messages []any) int {
	seenTool := false
	repaired := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringField(message, "role"))) {
		case "tool":
			seenTool = true
		case "assistant":
			hasToolCalls := false
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				hasToolCalls = true
			}
			if (hasToolCalls || seenTool) && !hasReasoningContent(message) && repaired < maxCompatReasoningRepairs {
				message["reasoning_content"] = compatReasoningPlaceholder
				repaired++
			}
			if hasToolCalls {
				seenTool = true
			}
		}
	}
	return repaired
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
