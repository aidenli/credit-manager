package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// filterUsedAuthsByLiveAccounts drops ledger identities whose account no longer
// exists on the host. The boolean reports whether the live set was known; when it
// is false the input is returned untouched so a host hiccup cannot hide accounts.
func filterUsedAuthsByLiveAccounts(ctx context.Context, svc *service.Service, used []store.UsageAuthSummary) ([]store.UsageAuthSummary, bool) {
	if len(used) == 0 {
		return used, false
	}
	live, err := svc.HostAuthIDs(ctx)
	if err != nil || len(live) == 0 {
		return used, false
	}
	out := make([]store.UsageAuthSummary, 0, len(used))
	for _, item := range used {
		if _, ok := live[strings.TrimSpace(item.AuthID)]; ok {
			out = append(out, item)
			continue
		}
		// Older rows may only carry an auth_index; match that too.
		if _, ok := live[strings.TrimSpace(item.AuthIndex)]; ok {
			out = append(out, item)
		}
	}
	return out, true
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
	// The ledger is historical: it still lists accounts that were deleted or
	// re-added. Keep only accounts the host currently holds so the console filter
	// does not offer dead entries. This is best-effort — if the host cannot be
	// asked, show everything rather than silently hiding real accounts.
	usedAuths, liveAuthsKnown := filterUsedAuthsByLiveAccounts(ctx, svc, usedAuths)
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
	return jsonOK(map[string]any{
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
		// Tells the console whether the account list was narrowed to live
		// accounts, so it can explain why a previously used account is absent.
		"used_auths_live_only": liveAuthsKnown,
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
	return jsonOK(keyViewWithBindings(key, bindings)), nil
}
