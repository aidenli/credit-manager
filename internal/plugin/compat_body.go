package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
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

// stripCompatEncryptedReasoning removes the account-bound reasoning state from a
// replayed history so another credential can serve it.
//
// `encrypted_content` is opaque and bound to the upstream identity that produced
// it: replayed through a different account it fails hydration with
//
//	The encrypted content for item amsg_… could not be verified.
//	Reason: encrypted content hydration failed. (code thinking_signature_invalid)
//
// and the client keeps replaying the same items, so the conversation cannot
// recover. The item `id` goes with the blob because an id minted by another
// account has no match upstream either. Readable `summary` text and any plain
// reasoning text stay, so the model still sees what it reasoned - it just cannot
// hydrate the exact chain.
func stripCompatEncryptedReasoning(body []byte) ([]byte, int) {
	if len(body) == 0 {
		return body, 0
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body, 0
	}
	stripped := 0
	if input, ok := payload["input"].([]any); ok {
		stripped += stripEncryptedReasoningEntries(input)
	}
	if messages, ok := payload["messages"].([]any); ok {
		stripped += stripEncryptedReasoningEntries(messages)
	}
	if nested, ok := payload["reasoning"].(map[string]any); ok {
		if _, ok := nested["encrypted_content"]; ok {
			delete(nested, "encrypted_content")
			stripped++
		}
	}
	if stripped == 0 {
		return body, 0
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, 0
	}
	return rewritten, stripped
}

// stripEncryptedReasoningEntries strips account-bound reasoning state from a
// list of Responses items or Chat Completions messages.
func stripEncryptedReasoningEntries(items []any) int {
	stripped := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := item["encrypted_content"]; ok {
			delete(item, "encrypted_content")
			delete(item, "id")
			stripped++
		}
		if _, ok := item["reasoning_content"]; ok {
			// DeepSeek's thinking mode requires the field on replayed assistant
			// turns, so it is emptied rather than dropped; the repair below fills
			// it back in for the providers that need a value.
			item["reasoning_content"] = ""
			stripped++
		}
		if reasoning, ok := item["reasoning"].(map[string]any); ok {
			if _, ok := reasoning["encrypted_content"]; ok {
				delete(reasoning, "encrypted_content")
				stripped++
			}
		}
	}
	return stripped
}

// repairCompatReasoning makes an API-provider request satisfy the thinking-mode
// contract. Returns how many items were repaired.
//
// Only a Chat Completions body is repaired this way. The Responses shape must be
// left alone: the plugin's own provider there is Codex, whose upstream validates
// every `input[]` item and answers
//
//	Unknown parameter: 'input[194].reasoning_content'. (code unknown_parameter)
//
// for a field it does not define. A Responses conversation already carries its
// own `reasoning` items and the host translator turns them into the provider's
// reasoning field, so the placeholder is only needed on the Chat Completions
// history a Codex conversation is replayed as.
func repairCompatReasoning(payload map[string]any) int {
	if messages, ok := payload["messages"].([]any); ok {
		return repairChatReasoning(messages)
	}
	return 0
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

// compatRequestKey fingerprints the caller credential so reasoning state that
// failed hydration can be remembered per Key without storing the Key itself.
func compatRequestKey(headers http.Header) string {
	if headers == nil {
		return ""
	}
	token := strings.TrimSpace(headers.Get("Authorization"))
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// dispatcherServedByAPI reports whether the host dispatched this attempt to an
// OpenAI-compatible API provider, i.e. the bound key's fallback rather than one
// of its own accounts.
func dispatcherServedByAPI(req pluginapi.ExecutorRequest) bool {
	return service.IsAPIProviderAuth(strings.TrimSpace(req.AuthProvider), requestAuthID(req))
}

// compatTargetsAPIProvider reports whether this attempt may reach an
// OpenAI-compatible API provider, even though the plugin selected one of the
// key's own OAuth accounts.
//
// The host's conductor is not bound by the plugin's pick: with round-robin
// routing and the request-scoped `continue` rules, an overloaded account makes it
// retry the same request on another credential of the same model, which can be
// the OpenAI-compatible provider the candidate window carries. The plugin learns
// that from the scheduler pick, so it records the models whose window offers one
// and repairs those bodies too. Without it the repair never ran on such
// attempts - the plugin saw `auth_provider=codex` - and the provider answered
// with the thinking-mode rejection this file exists to prevent.
func compatTargetsAPIProvider(req pluginapi.ExecutorRequest) bool {
	if dispatcherServedByAPI(req) {
		return true
	}
	if apiTargetForModels(req.Model) {
		return true
	}
	return false
}

// pickCandidatesServeAPI reports whether any provider key of a scheduling window
// belongs to an OpenAI-compatible API provider.
func pickCandidatesServeAPI(providers []string, candidates []service.AuthPickCandidate) bool {
	for _, provider := range providers {
		if isAPIProviderKey(provider) {
			return true
		}
	}
	for _, candidate := range candidates {
		if isAPIProviderKey(candidate.Provider) {
			return true
		}
	}
	return false
}

func isAPIProviderKey(provider string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(provider)), "openai-compatible")
}
