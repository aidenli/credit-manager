package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// markUsedAuthsDisabled applies the host switch to ledger identities. A row is
// matched on whichever identifier the ledger kept: auth_id normally, auth_index
// for older rows that never captured one. Identities the host does not know
// (accounts that were removed) keep Disabled=false, i.e. they are not claimed to
// be switched off.
func markUsedAuthsDisabled(used []store.UsageAuthSummary, disabled map[string]bool) {
	for i := range used {
		item := &used[i]
		for _, id := range []string{item.AuthID, item.AuthIndex} {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if isDisabled, ok := disabled[id]; ok {
				item.Disabled = isDisabled
				break
			}
		}
	}
}

func getOverview(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	filter, err := usageFilterFromQuery(query, 500)
	if err != nil {
		return jsonErr(http.StatusBadRequest, err.Error()), nil
	}
	keys, err := svc.Store().ListPluginKeys(ctx, 500)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	pricing, err := svc.Store().ListPricingRules(ctx)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	byKey, err := svc.Store().SummarizeUsageByKeyFiltered(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	byModel, err := svc.Store().SummarizeUsageByModelFiltered(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	usedModels, err := svc.Store().SummarizeUsageByModelFiltered(ctx, store.UsageFilter{})
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	usedAuths, err := svc.Store().ListUsedAuths(ctx)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	// Mark which accounts are currently switched on so the console can show them
	// first. Best-effort: on failure the flag stays false and nothing is claimed.
	if disabled, disabledErr := svc.AuthFileDisabled(ctx); disabledErr == nil {
		markUsedAuthsDisabled(usedAuths, disabled)
	}
	recent, err := svc.Store().ListUsage(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	keyViews, err := keyViewsWithBindings(ctx, svc.Store(), keys)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	usageViews := make([]map[string]any, 0, len(recent))
	for _, u := range recent {
		usageViews = append(usageViews, usageView(u))
	}
	// no-store: this payload is rebuilt per request and the console would
	// otherwise serve a cached copy, silently missing new fields such as the
	// per-account disabled flag.
	return jsonOKNoStore(map[string]any{
		"status":         "ok",
		"plugin":         service.PluginID,
		"version":        service.PluginVersion,
		"data_dir":       svc.Config().DataDir,
		"keys":           keyViews,
		"pricing":        pricing,
		"usage_by_key":   byKey,
		"usage_by_model": byModel,
		"used_models":    usedModels,
		"used_auths":     usedAuths,
		"filters":        usageFilterView(filter),
		"recent_usage":   usageViews,
	}), nil
}

func getBalance(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	keyID := firstQuery(query, "key_id")
	if keyID == "" {
		return jsonErr(http.StatusBadRequest, "key_id query is required"), nil
	}
	key, err := svc.Store().GetPluginKey(ctx, keyID)
	if err != nil {
		return jsonErr(http.StatusNotFound, err.Error()), nil
	}
	// Keep the response shape identical to the key list so clients that render a
	// key view cannot mistake a missing field for "no bindings".
	bindings, err := svc.Store().ListKeyAuthBindings(ctx, key.ID)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	return jsonOKNoStore(keyViewWithBindings(key, bindings)), nil
}
