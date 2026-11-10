package plugin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/store"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
)

// The host uses PascalCase fields. Unlike decodeHostUsageRecord, this typed
// whitelist never materializes unknown fields as RawMessage (notably Failure,
// Source, ResponseHeaders, Generate, or any body). Oversized events are dropped
// before decoding; host/ABI allocations before this callback are outside it.
const maxStatisticsEventBytes = 1 << 20

// All auth_only traffic, including images, bypasses reservation and settlement.
// Keep authentication, model permissions, and the existing concurrency policy.
func interceptStatisticsRequest(svc *service.Service, raw []byte) ([]byte, error) {
	var req struct {
		RequestID, RequestedModel, Model string
		Headers                          http.Header
		Metadata                         struct {
			CallerScope          string `json:"caller_scope"`
			SelectedAuthID       string `json:"selected_auth_id"`
			AuthID               string `json:"auth_id"`
			SelectedAuthProvider string `json:"selected_auth_provider"`
			AuthProvider         string `json:"auth_provider"`
		}
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return admissionResponse(quotaRejectResponse(http.StatusServiceUnavailable, service.ErrAuthAdmission.Error()))
	}
	requestID := strings.TrimSpace(req.RequestID)
	respond := func(response pluginapi.RequestInterceptResponse) ([]byte, error) {
		if response.Terminate {
			svc.CompleteAuthAdmission(requestID)
			releaseKeySlot(requestID)
		}
		return admissionResponse(response)
	}
	auth := store.AuthIdentity{AuthID: firstNonEmpty(req.Metadata.SelectedAuthID, req.Metadata.AuthID), Provider: firstNonEmpty(req.Metadata.SelectedAuthProvider, req.Metadata.AuthProvider)}
	if warmup, err := verifyWarmupAdmission(req.Headers, auth); warmup {
		if err != nil {
			return respond(quotaRejectResponse(http.StatusServiceUnavailable, err.Error()))
		}
		return respond(pluginapi.RequestInterceptResponse{})
	}
	if err := svc.VerifyAuthAdmission(requestID, req.Headers.Get(authAdmissionHeader), auth.AuthID, auth.Provider); err != nil {
		return respond(quotaRejectResponse(http.StatusServiceUnavailable, err.Error()))
	}
	ctx := context.Background()
	key, _, err := svc.ResolveIdentity(ctx, req.Headers, map[string]any{service.CallerScopeMetadataKey: req.Metadata.CallerScope})
	if err != nil {
		return respond(quotaRejectResponse(http.StatusUnauthorized, err.Error()))
	}
	model := firstNonEmpty(req.RequestedModel, req.Model)
	// Only authenticated, plugin-side rejections are recorded here. No body,
	// headers, or upstream error payload is retained, and no hold is created.
	reject := func(code, reason string, response pluginapi.RequestInterceptResponse) ([]byte, error) {
		logCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := svc.Store().RecordRequestRejection(logCtx, store.RequestRejection{
			RequestID: requestID, CallerID: key.CallerID, PluginKeyID: key.ID,
			Model: model, ReasonCode: code, Reason: reason, StatusCode: response.StatusCode,
		}); err != nil {
			// Observability must not replace the original rejection or allow traffic.
			hostLog("warn", "credit-manager could not record request rejection", map[string]any{
				"plugin_key_id": key.ID, "reason_code": code,
			}, "")
		}
		return respond(response)
	}
	if !store.ModelAllowed(key, model) {
		return reject("model_not_allowed", store.ErrModelNotAllowed.Error(), modelNotAllowedRejectResponse(store.ErrModelNotAllowed.Error()))
	}
	disabled, err := svc.ModelDisabled(ctx, model)
	if err != nil {
		return reject("model_policy_unavailable", "model policy unavailable", quotaRejectResponse(http.StatusServiceUnavailable, "model policy unavailable"))
	}
	if disabled {
		return reject("model_disabled", store.ErrModelDisabled.Error(), modelDisabledRejectResponse(store.ErrModelDisabled.Error()))
	}
	if requestID == "" {
		return reject("request_identity_unavailable", "request identity unavailable", quotaRejectResponse(http.StatusServiceUnavailable, "request identity unavailable"))
	}
	if !acquireKeySlot(requestID, key.ID, key.MaxConcurrentRequests) {
		return reject("key_concurrency_limit", store.ErrConcurrentLimit.Error(), quotaRejectResponse(http.StatusTooManyRequests, store.ErrConcurrentLimit.Error()))
	}
	return respond(pluginapi.RequestInterceptResponse{})
}

type statisticsTokenPart struct {
	TotalTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens int64
}

type statisticsUsageRecord struct {
	RequestID, APIKey, Model, Alias, Provider, ExecutorType   string
	AuthID, AuthIndex, AuthType, ServiceTier, ReasoningEffort string
	RequestedAt                                               time.Time
	Latency, TTFT                                             time.Duration
	Failed                                                    bool
	Detail                                                    struct {
		InputTokens, OutputTokens, ReasoningTokens, CachedTokens int64
		CacheReadTokens, CacheCreationTokens, TotalTokens        int64
		TokenBreakdown                                           struct {
			Input, Output statisticsTokenPart
			TotalTokens   int64
		}
	}
}

func decodeUsageStatistic(raw []byte) (service.UsageStatistic, bool) {
	if len(raw) == 0 || len(raw) > maxStatisticsEventBytes {
		return service.UsageStatistic{}, false
	}
	var wire statisticsUsageRecord
	if json.Unmarshal(raw, &wire) != nil {
		return service.UsageStatistic{}, false
	}
	detail := wire.Detail
	usage := money.TokenUsage{Input: detail.InputTokens, Output: detail.OutputTokens, Reasoning: detail.ReasoningTokens,
		Cached: detail.CachedTokens, CacheRead: detail.CacheReadTokens, CacheCreation: detail.CacheCreationTokens, ReportedTotal: detail.TotalTokens}
	if usage.Input == 0 {
		usage.Input = detail.TokenBreakdown.Input.TotalTokens
	}
	if usage.Output == 0 {
		usage.Output = detail.TokenBreakdown.Output.TotalTokens
	}
	if usage.Reasoning == 0 {
		usage.Reasoning = detail.TokenBreakdown.Output.ReasoningTokens
	}
	if usage.CacheRead == 0 {
		usage.CacheRead = detail.TokenBreakdown.Input.CacheReadTokens
	}
	if usage.CacheCreation == 0 {
		usage.CacheCreation = detail.TokenBreakdown.Input.CacheWriteTokens
	}
	if usage.ReportedTotal == 0 {
		usage.ReportedTotal = detail.TokenBreakdown.TotalTokens
	}
	return service.UsageStatistic{
		EventID: strings.TrimSpace(wire.RequestID), Principal: strings.TrimSpace(wire.APIKey), Model: firstNonEmpty(wire.Model, wire.Alias),
		Provider: strings.TrimSpace(wire.Provider), ExecutorType: strings.TrimSpace(wire.ExecutorType),
		AuthID: strings.TrimSpace(wire.AuthID), AuthIndex: strings.TrimSpace(wire.AuthIndex), AuthType: strings.TrimSpace(wire.AuthType),
		ServiceTier: strings.TrimSpace(wire.ServiceTier), ReasoningEffort: strings.TrimSpace(wire.ReasoningEffort),
		RequestedAt: wire.RequestedAt, Latency: wire.Latency, TTFT: wire.TTFT, Failed: wire.Failed, Usage: usage,
	}, true
}
