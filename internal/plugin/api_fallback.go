package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// apiFallback replays one failed OAuth attempt on the configured API provider.
//
// The scheduler-time fallback (service.PickAuthForKey) only fires when no bound
// account is offered at all, and it can only pick an API provider that the host
// put into the candidate window, which the host does only for models the
// provider aliases. Neither helps when the account was offered, ran, and failed
// with 429/5xx/overload. This executor-time fallback covers that case: the plugin
// pins the replay to the API provider itself and rewrites the model, so any
// model can fall back.
//
// It fires at most once per request and only while nothing has reached the
// client yet, because a half-answered stream cannot be restarted invisibly.
type apiFallback struct {
	svc      *service.Service
	key      store.PluginKey
	provider string
	model    string
	used     bool
}

func newAPIFallback(svc *service.Service, key store.PluginKey) *apiFallback {
	fb := &apiFallback{svc: svc, key: key}
	if svc == nil {
		return fb
	}
	cfg := svc.Config().APIFallback
	fb.provider = strings.TrimSpace(cfg.Provider)
	fb.model = strings.TrimSpace(cfg.Model)
	return fb
}

// take decides whether the failed attempt described by req should be replayed
// on the API provider. When it should, req and body are rewritten in place for
// the replay and true is returned.
func (fb *apiFallback) take(ctx context.Context, req *rpcExecutorRequest, body *[]byte, failure string) bool {
	if fb == nil || fb.used || fb.provider == "" || fb.model == "" || fb.svc == nil {
		return false
	}
	if !fb.svc.AuthFallbackOn() {
		return false
	}
	// An attempt that already ran on an API provider has nothing to fall to.
	if service.IsAPIProviderAuth(req.AuthProvider, req.AuthID) {
		return false
	}
	if !isFallbackWorthyFailure(failure) {
		return false
	}
	fb.used = true
	originalModel := req.Model
	next := *req
	next.ExecutorRequest.Model = fb.model
	next.ExecutorRequest.AuthProvider = fb.provider
	next.ExecutorRequest.AuthID = ""
	*req = next
	*body = apiFallbackBody(*body, fb.model)
	hostLog("warn", "credit-manager falling back to API provider: model="+originalModel+" -> "+fb.provider+"/"+fb.model,
		map[string]any{"model": originalModel, "provider": fb.provider}, req.HostCallbackID)
	fb.svc.RecordUpstreamFallback(ctx, fb.key, fb.provider, originalModel)
	return true
}

// apiFallbackBody prepares a body for the API provider: the account-bound
// reasoning state and the structured-output fields it rejects are removed, and
// the model is replaced with the one the provider serves.
func apiFallbackBody(body []byte, model string) []byte {
	body, _ = stripCompatEncryptedReasoning(body)
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
	for _, marker := range fallbackWorthyMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isStreamPreamble reports whether a Responses stream chunk only announces the
// response. Such chunks are held back until real output arrives, so a failure
// right after them can still be retried without the client noticing.
func isStreamPreamble(payload []byte) bool {
	text := string(payload)
	if strings.Contains(text, "response.output") || strings.Contains(text, "response.completed") ||
		strings.Contains(text, "response.failed") || strings.Contains(text, "error") {
		return false
	}
	return strings.Contains(text, "response.created") || strings.Contains(text, "response.in_progress")
}
