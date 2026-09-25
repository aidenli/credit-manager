package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
)

// OAuth intelligence probe endpoints. The console renders one card per account,
// so the state endpoint answers with the schedule plus every account and its
// latest result.

// oauthTestAccountsView is the payload behind the console's account cards.
type oauthTestAccountsView struct {
	Settings      any                        `json:"settings"`
	Accounts      []service.OAuthTestAccount `json:"accounts"`
	ServerTimeUTC string                     `json:"server_time_utc"`
	Question1     string                     `json:"question1"`
	Question2     string                     `json:"question2"`
}

// oauthTestState answers the card list. `?auth_ids=a,b` narrows it to the
// accounts a poll is interested in, so the console can refresh only the cards
// that are still running instead of rebuilding the whole grid.
func oauthTestState(ctx context.Context, svc *service.Service, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	settings, err := svc.OAuthTestSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, err.Error()), nil
	}
	accounts, err := svc.OAuthTestAccounts(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, err.Error()), nil
	}
	if wanted := oauthTestRequestedAuthIDs(req.Query); len(wanted) > 0 {
		filtered := make([]service.OAuthTestAccount, 0, len(wanted))
		for _, account := range accounts {
			if wanted[strings.TrimSpace(account.AuthID)] {
				filtered = append(filtered, account)
			}
		}
		accounts = filtered
	}
	question1, question2 := service.OAuthTestQuestions()
	return jsonOKNoStore(oauthTestAccountsView{
		Settings:      settings,
		Accounts:      accounts,
		ServerTimeUTC: time.Now().UTC().Format(time.RFC3339),
		Question1:     question1,
		Question2:     question2,
	}), nil
}

// oauthTestRequestedAuthIDs parses the comma-separated filter. An empty result
// means "every account".
func oauthTestRequestedAuthIDs(query url.Values) map[string]bool {
	if len(query) == 0 {
		return nil
	}
	raw := strings.Join(query["auth_ids"], ",")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	wanted := map[string]bool{}
	for _, value := range strings.Split(raw, ",") {
		if id := strings.TrimSpace(value); id != "" {
			wanted[id] = true
		}
	}
	return wanted
}

func updateOAuthTestSettings(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	current, err := svc.OAuthTestSettings(ctx)
	if err != nil {
		return jsonErrNoStore(http.StatusServiceUnavailable, err.Error()), nil
	}
	var req struct {
		Enabled           *bool   `json:"enabled"`
		HourUTC           *int    `json:"hour_utc"`
		MinuteUTC         *int    `json:"minute_utc"`
		Model             *string `json:"model"`
		ThinkingIntensity *string `json:"thinking_intensity"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.HourUTC != nil {
		current.HourUTC = *req.HourUTC
	}
	if req.MinuteUTC != nil {
		current.MinuteUTC = *req.MinuteUTC
	}
	if req.Model != nil {
		current.Model = strings.TrimSpace(*req.Model)
	}
	if req.ThinkingIntensity != nil {
		current.ThinkingIntensity = strings.TrimSpace(*req.ThinkingIntensity)
	}
	updated, err := svc.UpdateOAuthTestSettings(ctx, current)
	if err != nil {
		return jsonErrNoStore(http.StatusBadRequest, err.Error()), nil
	}
	return jsonOKNoStore(updated), nil
}

// runOAuthTests probes every account, or the one named in the body. The per-card
// button sends auth_id; the toolbar button sends nothing.
func runOAuthTests(ctx context.Context, svc *service.Service, body []byte) (pluginapi.ManagementResponse, error) {
	var req struct {
		AuthID string `json:"auth_id"`
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal(body, &req); err != nil {
			return jsonErrNoStore(http.StatusBadRequest, "invalid json"), nil
		}
	}
	started, err := svc.StartOAuthTest(ctx, req.AuthID)
	if err != nil {
		return jsonErrNoStore(http.StatusConflict, err.Error()), nil
	}
	return jsonOKNoStore(map[string]any{"started": started}), nil
}

func stopOAuthTests(_ context.Context, svc *service.Service) (pluginapi.ManagementResponse, error) {
	svc.StopOAuthTest()
	return jsonOKNoStore(map[string]any{"stopped": true}), nil
}
