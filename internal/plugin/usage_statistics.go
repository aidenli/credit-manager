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
			CallerScope string `json:"caller_scope"`
		}
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	ctx := context.Background()
	key, _, err := svc.ResolveIdentity(ctx, req.Headers, map[string]any{service.CallerScopeMetadataKey: req.Metadata.CallerScope})
	if err != nil {
		return okEnvelope(quotaRejectResponse(http.StatusUnauthorized, err.Error()))
	}
	model := firstNonEmpty(req.RequestedModel, req.Model)
	if !store.ModelAllowed(key, model) {
		return okEnvelope(modelNotAllowedRejectResponse(store.ErrModelNotAllowed.Error()))
	}
	disabled, err := svc.ModelDisabled(ctx, model)
	if err != nil {
		return okEnvelope(quotaRejectResponse(http.StatusServiceUnavailable, "model policy unavailable"))
	}
	if disabled {
		return okEnvelope(modelDisabledRejectResponse(store.ErrModelDisabled.Error()))
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return okEnvelope(quotaRejectResponse(http.StatusServiceUnavailable, "request identity unavailable"))
	}
	if !acquireKeySlot(requestID, key.ID, key.MaxConcurrentRequests) {
		return okEnvelope(quotaRejectResponse(http.StatusTooManyRequests, store.ErrConcurrentLimit.Error()))
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
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
