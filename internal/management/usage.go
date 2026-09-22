package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func listUsage(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	pageSize := queryInt(query, "page_size", queryInt(query, "limit", 10))
	if pageSize > 200 {
		pageSize = 200
	}
	filter, err := usageFilterFromQuery(query, pageSize)
	if err != nil {
		return jsonErr(http.StatusBadRequest, err.Error()), nil
	}
	filter.Limit = pageSize
	page := queryInt(query, "page", 1)
	if page < 1 {
		page = 1
	}
	total, err := svc.Store().CountUsage(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	totalPages := (total + int64(filter.Limit) - 1) / int64(filter.Limit)
	if totalPages == 0 {
		page = 1
	} else if int64(page) > totalPages {
		page = int(totalPages)
	}
	filter.Offset = (page - 1) * filter.Limit
	items, err := svc.Store().ListUsage(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, usageView(item))
	}
	// no-store: the console renders these straight into tables and would
	// otherwise show a cached page after a refresh.
	return jsonOKNoStore(map[string]any{
		"items":       out,
		"page":        page,
		"page_size":   filter.Limit,
		"total":       total,
		"total_pages": totalPages,
	}), nil
}

func usageSummary(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	filter, err := usageFilterFromQuery(query, 100)
	if err != nil {
		return jsonErr(http.StatusBadRequest, err.Error()), nil
	}
	byKey, err := svc.Store().SummarizeUsageByKeyFiltered(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	byModel, err := svc.Store().SummarizeUsageByModelFiltered(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	return jsonOKNoStore(map[string]any{
		"by_key":   byKey,
		"by_model": byModel,
		"filters":  usageFilterView(filter),
	}), nil
}

// listReleasedUsage reports the attempts whose hold was released instead of
// settled. They never reach the usage ledger, so before this endpoint a failed
// attempt could only be found as a quota_released audit event, without the model
// or the upstream error text the reason now carries.
func listReleasedUsage(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	limit := queryInt(query, "limit", 20)
	if limit > 200 {
		limit = 200
	}
	filter := store.ReleasedReservationFilter{
		CallerID:    firstQuery(query, "caller_id"),
		PluginKeyID: firstQuery(query, "plugin_key_id"),
		Model:       firstQuery(query, "model"),
		Limit:       limit,
	}
	var err error
	if filter.From, err = queryTime(query, "from"); err != nil {
		return jsonErr(http.StatusBadRequest, err.Error()), nil
	}
	if filter.To, err = queryTime(query, "to"); err != nil {
		return jsonErr(http.StatusBadRequest, err.Error()), nil
	}
	items, total, err := svc.Store().ListReleasedReservations(ctx, filter)
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		view := map[string]any{
			"id":             item.ID,
			"plugin_key_id":  item.PluginKeyID,
			"key_label":      item.KeyLabel,
			"model":          item.Model,
			"reason":         item.Reason,
			"reason_code":    releaseReasonCode(item.Reason),
			"held_micro_usd": item.HeldMicroUSD,
			"created_at":     item.CreatedAt,
			"released_at":    nil,
		}
		if item.ReleasedAt != nil {
			view["released_at"] = *item.ReleasedAt
		}
		out = append(out, view)
	}
	// no-store: the console renders released attempts straight into a table.
	return jsonOKNoStore(map[string]any{"items": out, "total": total}), nil
}

// releaseReasonCode splits the plugin's "<code>: <upstream error>" release reason
// so the console can show a stable code next to the upstream detail.
func releaseReasonCode(reason string) string {
	reason = strings.TrimSpace(reason)
	if idx := strings.Index(reason, ":"); idx > 0 {
		return strings.TrimSpace(reason[:idx])
	}
	return reason
}

func usageFilterFromQuery(query map[string][]string, fallbackLimit int) (store.UsageFilter, error) {
	filter := store.UsageFilter{
		CallerID:     firstQuery(query, "caller_id"),
		PluginKeyID:  firstQuery(query, "plugin_key_id"),
		Model:        firstQuery(query, "model"),
		Source:       firstQuery(query, "source"),
		AuthID:       firstQuery(query, "auth_id"),
		AuthProvider: firstQuery(query, "auth_provider"),
		AuthIndex:    firstQuery(query, "auth_index"),
		Limit:        queryInt(query, "limit", fallbackLimit),
	}
	var err error
	if filter.From, err = queryTime(query, "from"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.To, err = queryTime(query, "to"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.MinCostMicroUSD, err = queryMicroUSD(query, "min_cost_micro_usd"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.MaxCostMicroUSD, err = queryMicroUSD(query, "max_cost_micro_usd"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.MinTokens, err = queryOptionalInt64(query, "min_tokens"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.MaxTokens, err = queryOptionalInt64(query, "max_tokens"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.ServedAPI, err = queryOptionalBool(query, "served_api"); err != nil {
		return store.UsageFilter{}, err
	}
	if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return store.UsageFilter{}, errors.New("from must not be later than to")
	}
	if filter.MinCostMicroUSD != nil && filter.MaxCostMicroUSD != nil && *filter.MinCostMicroUSD > *filter.MaxCostMicroUSD {
		return store.UsageFilter{}, errors.New("min_cost_micro_usd must not be greater than max_cost_micro_usd")
	}
	if filter.MinTokens != nil && filter.MaxTokens != nil && *filter.MinTokens > *filter.MaxTokens {
		return store.UsageFilter{}, errors.New("min_tokens must not be greater than max_tokens")
	}
	return filter, nil
}

func usageFilterView(filter store.UsageFilter) map[string]any {
	view := map[string]any{
		"caller_id":          filter.CallerID,
		"plugin_key_id":      filter.PluginKeyID,
		"model":              filter.Model,
		"source":             filter.Source,
		"auth_id":            filter.AuthID,
		"auth_provider":      filter.AuthProvider,
		"auth_index":         filter.AuthIndex,
		"limit":              filter.Limit,
		"min_tokens":         filter.MinTokens,
		"max_tokens":         filter.MaxTokens,
		"served_api":         filter.ServedAPI,
		"min_cost_micro_usd": filter.MinCostMicroUSD,
		"max_cost_micro_usd": filter.MaxCostMicroUSD,
	}
	if filter.From != nil {
		view["from"] = filter.From.UTC()
	}
	if filter.To != nil {
		view["to"] = filter.To.UTC()
	}
	return view
}

func queryTime(query map[string][]string, key string) (*time.Time, error) {
	raw := firstQuery(query, key)
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		value, err = time.Parse("2006-01-02T15:04", raw)
	}
	if err != nil {
		return nil, errors.New(key + " must be RFC3339 or YYYY-MM-DDTHH:MM")
	}
	value = value.UTC()
	return &value, nil
}

func queryMicroUSD(query map[string][]string, key string) (*money.MicroUSD, error) {
	raw := firstQuery(query, key)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, errors.New(key + " must be a non-negative integer")
	}
	micro := money.MicroUSD(value)
	return &micro, nil
}

func queryOptionalInt64(query map[string][]string, key string) (*int64, error) {
	raw := firstQuery(query, key)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, errors.New(key + " must be a non-negative integer")
	}
	return &value, nil
}

func queryOptionalBool(query map[string][]string, key string) (*bool, error) {
	raw := strings.ToLower(firstQuery(query, key))
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		switch raw {
		case "0":
			value = false
		case "1":
			value = true
		default:
			return nil, errors.New(key + " must be a boolean")
		}
	}
	return &value, nil
}

func listAudit(ctx context.Context, svc *service.Service, query map[string][]string) (pluginapi.ManagementResponse, error) {
	items, err := svc.Store().ListAuditEventsFiltered(ctx, store.AuditFilter{
		CallerID:    firstQuery(query, "caller_id"),
		PluginKeyID: firstQuery(query, "plugin_key_id"),
		Limit:       queryInt(query, "limit", 100),
	})
	if err != nil {
		return jsonErr(http.StatusInternalServerError, err.Error()), nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, auditView(item))
	}
	// no-store: audit rows are read-only history rendered into a table.
	return jsonOKNoStore(map[string]any{"items": out}), nil
}
