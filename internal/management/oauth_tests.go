package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
)

// OAuth intelligence probe endpoints. They expose the schedule, the latest run,
// and manual start/stop. The probe itself lives in the plugin because only the
// plugin can pin a request to one account.

func getOAuthTestSettings(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	settings, err := svc.OAuthTestSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, err.Error()), nil
	}
	return jsonOKNoStore(settings), nil
}

func updateOAuthTestSettings(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	current, err := svc.OAuthTestSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, err.Error()), nil
	}
	var req struct {
		Enabled           *bool   `json:"enabled"`
		IntervalMinutes   *int    `json:"interval_minutes"`
		Model             *string `json:"model"`
		ThinkingIntensity *string `json:"thinking_intensity"`
		Prompt            *string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.IntervalMinutes != nil {
		current.IntervalMinutes = *req.IntervalMinutes
	}
	if req.Model != nil {
		current.Model = strings.TrimSpace(*req.Model)
	}
	if req.ThinkingIntensity != nil {
		current.ThinkingIntensity = strings.TrimSpace(*req.ThinkingIntensity)
	}
	if req.Prompt != nil {
		current.Prompt = strings.TrimSpace(*req.Prompt)
	}
	updated, err := svc.UpdateOAuthTestSettings(ctx, current)
	if err != nil {
		return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
	}
	return jsonOKNoStore(updated), nil
}

// runOAuthTests starts a sweep and answers with the run, so the console can
// render its progress immediately.
func runOAuthTests(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	run, err := svc.StartOAuthTest(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusConflict, err.Error()), nil
	}
	return jsonOKNoStore(run), nil
}

func stopOAuthTests(_ context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	svc.StopOAuthTest()
	return jsonOKNoStore(map[string]any{"stopped": true}), nil
}

func latestOAuthTests(ctx context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	run, err := svc.LatestOAuthTest(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusInternalServerError, err.Error()), nil
	}
	return jsonOKNoStore(run), nil
}
