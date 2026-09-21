package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// HostCallbackIDContextKey identifies the host callback associated with a management request.
// The host will populate it when management requests expose callback metadata.
type HostCallbackIDContextKey struct{}

func getAuthQuotas(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	hostCallbackID, _ := ctx.Value(HostCallbackIDContextKey{}).(string)
	overview, err := svc.AuthQuotaOverview(ctx, hostCallbackID, service.AuthQuotaFilter{
		Page:     queryInt(query, "page", 1),
		PageSize: queryInt(query, "page_size", service.AuthQuotaDefaultPageSize),
		Provider: firstQuery(query, "provider"),
		Q:        firstQuery(query, "q"),
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unavailable") {
			return jsonErrNoStore(http.StatusServiceUnavailable, "auth quota information unavailable"), nil
		}
		return jsonErrNoStore(http.StatusInternalServerError, "auth quota information failed"), nil
	}
	return jsonOKNoStore(overview), nil
}

func refreshAuthQuota(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		Provider  string `json:"provider"`
		AuthID    string `json:"auth_id"`
		AuthIndex string `json:"auth_index"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	if strings.TrimSpace(req.AuthID) == "" && strings.TrimSpace(req.AuthIndex) == "" {
		return jsonErrNoStore(http.StatusBadRequest, "auth_id or auth_index is required"), nil
	}
	hostCallbackID, _ := ctx.Value(HostCallbackIDContextKey{}).(string)
	item, err := svc.RefreshAuthQuota(ctx, hostCallbackID, req.Provider, req.AuthID, req.AuthIndex)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not found") {
			return jsonErrNoStore(http.StatusNotFound, "auth quota not found"), nil
		}
		if strings.Contains(msg, "unavailable") {
			return jsonErrNoStore(http.StatusServiceUnavailable, "auth quota information unavailable"), nil
		}
		return jsonErrNoStore(http.StatusInternalServerError, "auth quota refresh failed"), nil
	}
	return jsonOKNoStore(map[string]any{"item": item}), nil
}

func warmupAuthQuota(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		Provider  string   `json:"provider"`
		AuthID    string   `json:"auth_id"`
		AuthIndex string   `json:"auth_index"`
		Models    []string `json:"models"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	if strings.TrimSpace(req.AuthID) == "" && strings.TrimSpace(req.AuthIndex) == "" {
		return jsonErrNoStore(http.StatusBadRequest, "auth_id or auth_index is required"), nil
	}
	item, err := svc.WarmupAuthQuota(ctx, req.Provider, req.AuthID, req.AuthIndex, req.Models)
	if err != nil {
		message := strings.ToLower(err.Error())
		switch {
		case strings.Contains(message, "not found"):
			return jsonErrNoStore(http.StatusNotFound, "auth quota not found"), nil
		case strings.Contains(message, "disabled"), strings.Contains(message, "not configured"), strings.Contains(message, "unavailable"):
			return jsonErrNoStore(http.StatusServiceUnavailable, "auth quota warmup unavailable"), nil
		case strings.Contains(message, "requires a fresh"):
			return jsonErrNoStore(http.StatusConflict, "refresh auth quota before warming up"), nil
		case strings.Contains(message, "requires at least one model"), strings.Contains(message, "no selected model"):
			return jsonErrNoStore(http.StatusConflict, err.Error()), nil
		default:
			return jsonErrNoStore(http.StatusInternalServerError, "auth quota warmup failed"), nil
		}
	}
	return jsonOKNoStore(map[string]any{"item": item}), nil
}

func getAuthWarmupSettings(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	settings, err := svc.AuthWarmupSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, "auth quota warmup unavailable"), nil
	}
	return jsonOKNoStore(settings), nil
}

func updateAuthWarmupSettings(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var settings store.AuthWarmupSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	updated, err := svc.UpdateAuthWarmupSettings(ctx, settings)
	if err != nil {
		return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
	}
	return jsonOKNoStore(updated), nil
}

// getAuthAccounts lists every credential the host holds, OAuth or API-key backed,
// for the key binding picker. Unlike auth-quotas it is not limited to accounts
// that expose an upstream quota API.
func getAuthAccounts(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	accounts, err := svc.AuthAccounts(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, "auth accounts unavailable"), nil
	}
	return jsonOKNoStore(map[string]any{"items": accounts}), nil
}

// sessionAffinitySettingsView is the JSON shape the console renders. TTL is a
// friendly string ("1h") rather than a nanosecond count.
type sessionAffinitySettingsView struct {
	Enabled   bool   `json:"enabled"`
	TTL       string `json:"ttl"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func sessionAffinityView(settings store.SessionAffinitySettings) sessionAffinitySettingsView {
	view := sessionAffinitySettingsView{Enabled: settings.Enabled, TTL: settings.TTL.String()}
	if settings.UpdatedAt != nil {
		view.UpdatedAt = settings.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return view
}

func getAuthSessionAffinitySettings(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	settings, err := svc.AuthSessionAffinitySettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, "session affinity settings unavailable"), nil
	}
	return jsonOKNoStore(sessionAffinityView(settings)), nil
}

func updateAuthSessionAffinitySettings(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		Enabled *bool   `json:"enabled"`
		TTL     *string `json:"ttl"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	current, err := svc.AuthSessionAffinitySettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, "session affinity settings unavailable"), nil
	}
	// Field-missing means "leave unchanged", mirroring the key update contract.
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.TTL != nil {
		parsed, parseErr := time.ParseDuration(strings.TrimSpace(*req.TTL))
		if parseErr != nil {
			return jsonErrNoStore(http.StatusBadRequest, "ttl must be a duration such as 1h or 30m"), nil
		}
		current.TTL = parsed
	}
	updated, err := svc.UpdateAuthSessionAffinitySettings(ctx, current)
	if err != nil {
		return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
	}
	return jsonOKNoStore(sessionAffinityView(updated)), nil
}

// authFallbackSettingsView is the JSON shape the console renders for the
// API-provider fallback of bound keys.
type authFallbackSettingsView struct {
	Enabled   bool   `json:"enabled"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func authFallbackView(settings store.AuthFallbackSettings) authFallbackSettingsView {
	view := authFallbackSettingsView{Enabled: settings.Enabled}
	if settings.UpdatedAt != nil {
		view.UpdatedAt = settings.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return view
}

func getAuthFallbackSettings(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	settings, err := svc.AuthFallbackSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, "auth fallback settings unavailable"), nil
	}
	return jsonOKNoStore(authFallbackView(settings)), nil
}

func updateAuthFallbackSettings(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	current, err := svc.AuthFallbackSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, "auth fallback settings unavailable"), nil
	}
	// Field-missing means "leave unchanged", mirroring the key update contract.
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	updated, err := svc.UpdateAuthFallbackSettings(ctx, current)
	if err != nil {
		return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
	}
	return jsonOKNoStore(authFallbackView(updated)), nil
}

func updateAuthQuotaConcurrency(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		Provider              string `json:"provider"`
		AuthID                string `json:"auth_id"`
		AuthIndex             string `json:"auth_index"`
		MaxConcurrentRequests *int64 `json:"max_concurrent_requests"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	authID := strings.TrimSpace(req.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(req.AuthIndex)
	}
	if authID == "" {
		return jsonErrNoStore(http.StatusBadRequest, "auth_id or auth_index is required"), nil
	}
	if req.MaxConcurrentRequests == nil {
		return jsonErrNoStore(http.StatusBadRequest, "max_concurrent_requests is required"), nil
	}
	if *req.MaxConcurrentRequests < 0 {
		return jsonErrNoStore(http.StatusBadRequest, "max_concurrent_requests must not be negative"), nil
	}
	item, err := svc.SetAuthConcurrencyLimit(ctx, req.Provider, authID, *req.MaxConcurrentRequests)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "required") || strings.Contains(msg, "negative") {
			return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
		}
		if strings.Contains(msg, "unavailable") {
			return jsonErrNoStore(http.StatusServiceUnavailable, "auth quota information unavailable"), nil
		}
		return jsonErrNoStore(http.StatusInternalServerError, "auth concurrency update failed"), nil
	}
	return jsonOKNoStore(map[string]any{"item": item}), nil
}

func updateAuthQuotaConcurrencyBatch(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		Provider              string                          `json:"provider"`
		Q                     string                          `json:"q"`
		MaxConcurrentRequests *int64                          `json:"max_concurrent_requests"`
		Items                 []service.AuthConcurrencyTarget `json:"items"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	if req.MaxConcurrentRequests == nil {
		return jsonErrNoStore(http.StatusBadRequest, "max_concurrent_requests is required"), nil
	}
	if *req.MaxConcurrentRequests < 0 {
		return jsonErrNoStore(http.StatusBadRequest, "max_concurrent_requests must not be negative"), nil
	}
	result, err := svc.SetAuthConcurrencyLimits(ctx, service.AuthQuotaFilter{Provider: req.Provider, Q: req.Q}, req.Items, *req.MaxConcurrentRequests)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "required") || strings.Contains(msg, "negative") {
			return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
		}
		if strings.Contains(msg, "unavailable") {
			return jsonErrNoStore(http.StatusServiceUnavailable, "auth quota information unavailable"), nil
		}
		return jsonErrNoStore(http.StatusInternalServerError, "auth concurrency batch update failed"), nil
	}
	return jsonOKNoStore(result), nil
}
