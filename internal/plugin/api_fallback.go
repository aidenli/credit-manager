package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// apiFallback replays recoverable failures before output, once per API provider.
// The shared model alias maps to each provider's own upstream model in CPA.
// Provider-local retries and cooldowns remain with the host; every replay stays
// forced to an allowed provider rather than exposing the key to unbound OAuth.
type apiFallback struct {
	svc           *service.Service
	key           store.PluginKey
	providers     []string
	model         string
	multi         bool
	loaded        bool
	remaining     []string
	originalModel string
}

func newAPIFallback(svc *service.Service, key store.PluginKey) *apiFallback {
	fb := &apiFallback{svc: svc, key: key}
	if svc != nil {
		cfg := svc.Config().APIFallback
		fb.providers = cfg.ProviderKeys()
		fb.model = strings.TrimSpace(cfg.Model)
		fb.multi = len(cfg.Providers) > 0
	}
	return fb
}

// take rewrites req and body for the next allowed replay. Callers must stop
// calling it once any output has reached the client.
func (fb *apiFallback) take(ctx context.Context, req *rpcExecutorRequest, body *[]byte, failure string) bool {
	if fb == nil || fb.svc == nil || len(fb.providers) == 0 || fb.model == "" ||
		!fb.svc.AuthFallbackOn() || ctx.Err() != nil || !isFallbackWorthyFailure(failure) {
		return false
	}
	if !fb.loaded {
		current := strings.ToLower(strings.TrimSpace(req.AuthProvider))
		if service.IsAPIProviderAuth(req.AuthProvider, req.AuthID) &&
			(!fb.multi || !slices.Contains(fb.providers, current)) {
			return false
		}
		fb.loaded = true
		fb.originalModel = req.Model
		fb.remaining = fb.providers
		if fb.multi {
			var err error
			fb.remaining, err = hostAPIFallbackOrder(fb.providers, current)
			if err != nil {
				hostLog("warn", "credit-manager cannot read API fallback priorities", nil, req.HostCallbackID)
				return false
			}
		}
	}
	if len(fb.remaining) == 0 {
		return false
	}
	provider := fb.remaining[0]
	fb.remaining = fb.remaining[1:]
	req.ExecutorRequest.Model = fb.model
	req.ExecutorRequest.AuthProvider = provider
	req.ExecutorRequest.AuthID = ""
	*body = apiFallbackBody(*body, fb.model)
	hostLog("warn", "credit-manager falling back to API provider: model="+fb.originalModel+" -> "+provider+"/"+fb.model,
		map[string]any{"model": fb.originalModel, "provider": provider}, req.HostCallbackID)
	fb.svc.RecordUpstreamFallback(ctx, fb.key, provider, fb.originalModel)
	return true
}

var apiFallbackCursor atomic.Uint64

// The SDK accepts only one forced provider, so order provider snapshots here
// without clearing the provider boundary. Host availability remains model-specific.
func hostAPIFallbackOrder(allowed []string, current string) ([]string, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	priorities := make(map[string]int)
	for _, entry := range response.Files {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == current || entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled") ||
			!slices.Contains(allowed, provider) || !strings.HasPrefix(provider, "openai-compatible-") {
			continue
		}
		if previous, ok := priorities[provider]; !ok || entry.Priority > previous {
			priorities[provider] = entry.Priority
		}
	}
	providers := make([]string, 0, len(priorities))
	for provider := range priorities {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool {
		if priorities[providers[i]] != priorities[providers[j]] {
			return priorities[providers[i]] > priorities[providers[j]]
		}
		return providers[i] < providers[j]
	})
	cursor := apiFallbackCursor.Add(1) - 1
	for start := 0; start < len(providers); {
		end := start + 1
		for end < len(providers) && priorities[providers[end]] == priorities[providers[start]] {
			end++
		}
		tier := append([]string(nil), providers[start:end]...)
		offset := int(cursor % uint64(len(tier)))
		copy(providers[start:end], append(tier[offset:], tier[:offset]...))
		start = end
	}
	return providers, nil
}

// apiFallbackBody prepares a body for the API provider: the account-bound
// reasoning state and the structured-output fields it rejects are removed, and
// the model is replaced with the one the provider serves.
func apiFallbackBody(body []byte, model string) []byte {
	body, _ = stripCompatEncryptedReasoning(body)
	body, _ = dropCompactionItems(body)
	body, _ = sanitizeCompatRequestBody(body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body
	}
	if _, ok := payload["model"]; !ok {
		return body
	}
	payload["model"] = model
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return rewritten
}

// isFallbackWorthyStatus reports whether an upstream HTTP status is the kind of
// account-side failure another provider can answer: timeouts, rate limits and
// server errors. Other 4xx statuses describe the request itself and would fail
// on any provider.
func isFallbackWorthyStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// fallbackWorthyMarkers are matched against the lower-cased failure text. They
// cover the codex in-stream terminal events (server_is_overloaded,
// usage_limit_reached), host-wrapped HTTP failures, and the account-bound
// reasoning rejection, which an API provider never sees because the replay is
// stripped.
var fallbackWorthyMarkers = []string{
	"overloaded", "server_is_overloaded", "service_unavailable", "service unavailable",
	"usage_limit", "rate_limit", "rate limit", "ratelimit", "too many requests", "quota",
	"status 408", "status 429", "status 500", "status 502", "status 503", "status 504",
	"internal_server_error", "internal server error", "bad gateway", "gateway timeout",
	"timeout", "timed out", "temporarily", "try again later",
	"auth_unavailable", "no auth available", "currently unavailable", "cooling",
	stripEncryptedRetryText, "could not be verified",
}

func isFallbackWorthyFailure(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	fields := strings.Fields(lower)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "status" {
			if status, err := strconv.Atoi(strings.TrimRight(fields[i+1], ":,")); err == nil && isFallbackWorthyStatus(status) {
				return true
			}
		}
	}
	for _, marker := range fallbackWorthyMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isStreamPreamble reports whether a Responses stream chunk only carries
// handshake events that announce the response without producing any content.
// Such chunks are held back until real output arrives, so a failure right after
// them (the codex overload / encrypted-reasoning rejection shape, which the host
// flushes the buffered handshake frames before) can still be retried or fallen
// back without the client noticing.
//
// A chunk may bundle several SSE "data:" lines, so it is preamble only when every
// event line it carries is a handshake event. The classification mirrors the
// host's own isCodexBootstrapBufferableEvent: it keys off the event type rather
// than scanning for substrings, because the response.created envelope legitimately
// embeds "error":null and an empty "output":[] that a substring test misreads as
// real content.
func isStreamPreamble(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return true
	}
	sawEvent := false
	for _, raw := range bytes.Split(trimmed, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte("id:")) ||
			bytes.HasPrefix(line, []byte("retry:")) || bytes.HasPrefix(line, []byte(":")) {
			// SSE comments and framing lines carry no event, so a chunk made only
			// of them is still safe to hold back.
			sawEvent = true
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			return false
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		sawEvent = true
		if !isPreambleEvent(data) {
			return false
		}
	}
	return sawEvent
}

// preambleEventTypes are the Responses handshake events that announce the stream
// without emitting content, mirroring the host's bootstrap buffer allow-list.
var preambleEventTypes = map[string]struct{}{
	"response.created":        {},
	"response.in_progress":    {},
	"response.queued":         {},
	"codex.rate_limits":       {},
	"codex.response.metadata": {},
	"keepalive":               {},
}

// isPreambleEvent reports whether one decoded SSE data event is a content-free
// handshake frame. An *.added announcement is preamble only while the item or
// part it announces is still empty; a reasoning item that already carries
// encrypted_content is real state and is not held.
func isPreambleEvent(data []byte) bool {
	var event struct {
		Type string `json:"type"`
		Item struct {
			Type             string          `json:"type"`
			EncryptedContent string          `json:"encrypted_content"`
			Content          json.RawMessage `json:"content"`
			Summary          json.RawMessage `json:"summary"`
			Arguments        string          `json:"arguments"`
		} `json:"item"`
		Part struct {
			Text string `json:"text"`
		} `json:"part"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return false
	}
	if _, ok := preambleEventTypes[event.Type]; ok {
		return true
	}
	switch event.Type {
	case "response.output_item.added":
		if event.Item.EncryptedContent != "" {
			return false
		}
		switch event.Item.Type {
		case "message", "reasoning":
			return isEmptyRawList(event.Item.Content) && isEmptyRawList(event.Item.Summary)
		case "function_call":
			return event.Item.Arguments == ""
		default:
			return false
		}
	case "response.content_part.added", "response.reasoning_summary_part.added":
		return strings.TrimSpace(event.Part.Text) == ""
	}
	return false
}

// isEmptyRawList reports whether a raw JSON value is absent, null, or an empty
// array, which is how an announced-but-empty content or summary field appears.
func isEmptyRawList(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var list []json.RawMessage
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return false
	}
	return len(list) == 0
}
