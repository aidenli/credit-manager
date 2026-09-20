package management

import (
	"context"
	"net/http"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
