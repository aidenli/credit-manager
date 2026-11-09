package store

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
)

func TestUsageCostMixedReportingAndFinancialIsolation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	keyA := newTestKey(t, ctx, st, PluginKeySpec{Kid: "usage-key-a", Label: "usage first",
		ModelTokenLimits: []ModelTokenLimit{{Model: "test-model", Total: ModelPeriodTokenLimit{Tokens: 100}}}})
	keyB := newTestKey(t, ctx, st, PluginKeySpec{Kid: "usage-key-b", Label: "actual first"})
	for _, fixture := range []struct {
		key      PluginKey
		id       string
		cost     money.MicroUSD
		provider string
	}{
		{keyA, "historical-magpie", 40, " MaGpIe "},
		{keyB, "ledger-paid", 60, "paid"},
	} {
		reservation, err := st.Reserve(ctx, reserveRequest(fixture.key, fixture.id, fixture.cost))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Settle(ctx, Settlement{LedgerID: fixture.id, ReservationID: reservation.ID,
			Model: "test-model", Usage: money.TokenUsage{Input: 10}, CostMicroUSD: fixture.cost,
			EstimatedCostMicroUSD: 999, ServedAPI: true, ServedProvider: fixture.provider}); err != nil {
			t.Fatal(err)
		}
	}
	// Snapshot every financial column, not only the display cost or row counts.
	financialState := func() map[string][][]any {
		t.Helper()
		state := make(map[string][][]any)
		for _, table := range []string{"callers", "plugin_keys", "reservations", "usage_ledger", "audit_events"} {
			rows, err := st.db.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				state[table] = append(state[table], values)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
		}
		return state
	}
	before := financialState()
	now := time.Now().UTC().Truncate(time.Millisecond)
	quotaBefore, err := st.ListModelTokenUsage(ctx, keyA.ID, keyA.ModelTokenLimits, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	pricing, emptyPricing := "reference-rule", ""
	statistics := []UsageEntry{
		{ID: "oauth", PluginKeyID: keyA.ID, Model: "oauth-model", EstimatedCostMicroUSD: 250, PricingRuleID: &pricing,
			Auth: AuthIdentity{Provider: "codex", Type: "oauth"}},
		{ID: "served-magpie", PluginKeyID: keyB.ID, Model: "magpie-model", EstimatedCostMicroUSD: 900,
			ServedAPI: true, ServedProvider: " MaGpIe ", Auth: AuthIdentity{Provider: "codex"}},
		{ID: "auth-magpie", PluginKeyID: keyB.ID, Model: "magpie-model", EstimatedCostMicroUSD: 800,
			Auth: AuthIdentity{Provider: " OpenAI-Compatible-Magpie "}},
		{ID: "missing-zero", PluginKeyID: keyA.ID, Model: "missing-model"},
		{ID: "missing-estimate", PluginKeyID: keyA.ID, Model: "missing-model", EstimatedCostMicroUSD: 25},
		// A non-null ID (even empty) is not missing; a priced zero is not unpriced.
		{ID: "priced-zero", PluginKeyID: keyB.ID, Model: "zero-model", PricingRuleID: &emptyPricing},
	}
	for i := range statistics {
		statistics[i].CallerID = keyA.CallerID
		statistics[i].Usage = money.TokenUsage{Input: 10, Output: 2}
		statistics[i].CreatedAt = now
	}
	if err := st.InsertUsageStatistics(ctx, statistics); err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListUsage(ctx, UsageFilter{})
	if err != nil || len(entries) != 8 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	wantCosts := map[string]struct {
		actual money.MicroUSD
		usage  money.MicroUSD
		status string
	}{
		"historical-magpie": {40, 40, "settled"}, "ledger-paid": {60, 60, "settled"},
		"oauth": {0, 250, "priced"}, "served-magpie": {0, 0, "free"}, "auth-magpie": {0, 0, "free"},
		"missing-zero": {0, 0, "unpriced"}, "missing-estimate": {0, 25, "unpriced"}, "priced-zero": {0, 0, "priced"},
	}
	for _, entry := range entries {
		want, ok := wantCosts[entry.ID]
		if !ok || entry.CostMicroUSD != want.actual || entry.UsageCostMicroUSD != want.usage || entry.UsageCostStatus != want.status {
			t.Fatalf("entry=%+v want=%+v", entry, want)
		}
	}
	ledger, err := st.GetUsage(ctx, "historical-magpie")
	if err != nil || ledger.CostMicroUSD != 40 || ledger.UsageCostMicroUSD != 40 || ledger.UsageCostStatus != "settled" {
		t.Fatalf("historical ledger=%+v err=%v", ledger, err)
	}
	// usageWhere is also used by ledger-only reads; Magpie never erases ledger spend.
	forty := money.MicroUSD(40)
	ledgerRows, err := st.listUsage(ctx, UsageFilter{MinCostMicroUSD: &forty, MaxCostMicroUSD: &forty}, "usage_ledger")
	if err != nil || len(ledgerRows) != 1 || ledgerRows[0].ID != "historical-magpie" {
		t.Fatalf("ledger-only cost filter=%+v err=%v", ledgerRows, err)
	}
	overview, err := st.UsageOverviewSummary(ctx)
	if err != nil || overview.RequestCount != 8 || overview.TotalTokens != 92 || overview.UsageCostMicroUSD != 375 || overview.UnpricedCount != 2 || overview.EstimatedCostMicroUSD != 2075 {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	all, err := st.SummarizeUsageFiltered(ctx, UsageFilter{})
	if err != nil || all.RequestCount != 8 || all.TotalTokens != 92 || all.CostMicroUSD != 100 || all.UsageCostMicroUSD != 375 || all.UnpricedCount != 2 || all.FallbackCount != 3 {
		t.Fatalf("summary=%+v err=%v", all, err)
	}
	keys, err := st.SummarizeUsageByKey(ctx, keyA.CallerID)
	if err != nil || len(keys) != 2 || keys[0].Label != keyA.Label || keys[0].CostMicroUSD != 40 || keys[0].UsageCostMicroUSD != 315 || keys[0].UnpricedCount != 2 || keys[1].CostMicroUSD != 60 || keys[1].UsageCostMicroUSD != 60 || keys[1].UnpricedCount != 0 {
		t.Fatalf("keys (usage cost order)=%+v err=%v", keys, err)
	}
	models, err := st.SummarizeUsageByModel(ctx, keyA.CallerID, "")
	if err != nil || len(models) != 5 || models[0].Model != "oauth-model" || models[0].UsageCostMicroUSD != 250 || models[0].CostMicroUSD != 0 {
		t.Fatalf("models (usage cost order)=%+v err=%v", models, err)
	}
	var modelUsage, modelActual money.MicroUSD
	var modelUnpriced int64
	for _, model := range models {
		modelUsage += model.UsageCostMicroUSD
		modelActual += model.CostMicroUSD
		modelUnpriced += model.UnpricedCount
	}
	if modelUsage != 375 || modelActual != 100 || modelUnpriced != 2 {
		t.Fatalf("model totals usage=%d actual=%d unpriced=%d", modelUsage, modelActual, modelUnpriced)
	}
	for _, grain := range []string{"hour", "day", "month"} {
		trend, err := st.SummarizeUsageTrendFiltered(ctx, UsageFilter{}, grain)
		if err != nil {
			t.Fatal(err)
		}
		var usage, actual money.MicroUSD
		var unpriced int64
		for _, point := range trend {
			usage += point.UsageCostMicroUSD
			actual += point.CostMicroUSD
			unpriced += point.UnpricedCount
		}
		if usage != 375 || actual != 100 || unpriced != 2 {
			t.Fatalf("%s trend=%+v", grain, trend)
		}
	}
	tokens, err := st.UsageTokenTrend(ctx, 30)
	if err != nil || len(tokens) != 1 || tokens[0].TotalTokens != 92 || tokens[0].UsageCostMicroUSD != 375 || tokens[0].UnpricedCount != 2 {
		t.Fatalf("token trend=%+v err=%v", tokens, err)
	}
	zero, one, hundred, oauthCost := money.MicroUSD(0), money.MicroUSD(1), money.MicroUSD(100), money.MicroUSD(250)
	for _, tc := range []struct {
		name     string
		filter   UsageFilter
		ids      []string
		usage    money.MicroUSD
		actual   money.MicroUSD
		unpriced int64
	}{
		{"zero", UsageFilter{MinCostMicroUSD: &zero, MaxCostMicroUSD: &zero}, []string{"auth-magpie", "missing-zero", "priced-zero", "served-magpie"}, 0, 0, 1},
		{"range", UsageFilter{MinCostMicroUSD: &one, MaxCostMicroUSD: &hundred}, []string{"historical-magpie", "ledger-paid", "missing-estimate"}, 125, 100, 1},
		{"oauth", UsageFilter{MinCostMicroUSD: &oauthCost, MaxCostMicroUSD: &oauthCost}, []string{"oauth"}, 250, 0, 0},
		{"statistics", UsageFilter{Source: "statistics"}, []string{"auth-magpie", "missing-estimate", "missing-zero", "oauth", "priced-zero", "served-magpie"}, 275, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := st.ListUsage(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			sort.Strings(ids)
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("ids=%v want=%v", ids, tc.ids)
			}
			count, err := st.CountUsage(ctx, tc.filter)
			if err != nil || count != int64(len(tc.ids)) {
				t.Fatalf("count=%d err=%v", count, err)
			}
			summary, err := st.SummarizeUsageFiltered(ctx, tc.filter)
			if err != nil || summary.RequestCount != count || summary.UsageCostMicroUSD != tc.usage || summary.CostMicroUSD != tc.actual || summary.UnpricedCount != tc.unpriced {
				t.Fatalf("filtered summary=%+v err=%v", summary, err)
			}
			keyRows, err := st.SummarizeUsageByKeyFiltered(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			var keyUsage, keyActual money.MicroUSD
			var keyUnpriced int64
			for _, row := range keyRows {
				keyUsage += row.UsageCostMicroUSD
				keyActual += row.CostMicroUSD
				keyUnpriced += row.UnpricedCount
			}
			modelRows, err := st.SummarizeUsageByModelFiltered(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			var modelUsage, modelActual money.MicroUSD
			var modelUnpriced int64
			for _, row := range modelRows {
				modelUsage += row.UsageCostMicroUSD
				modelActual += row.CostMicroUSD
				modelUnpriced += row.UnpricedCount
			}
			trend, err := st.SummarizeUsageTrendFiltered(ctx, tc.filter, "day")
			if err != nil {
				t.Fatal(err)
			}
			var trendUsage, trendActual money.MicroUSD
			var trendUnpriced int64
			for _, row := range trend {
				trendUsage += row.UsageCostMicroUSD
				trendActual += row.CostMicroUSD
				trendUnpriced += row.UnpricedCount
			}
			if keyUsage != tc.usage || modelUsage != tc.usage || trendUsage != tc.usage || keyActual != tc.actual || modelActual != tc.actual || trendActual != tc.actual || keyUnpriced != tc.unpriced || modelUnpriced != tc.unpriced || trendUnpriced != tc.unpriced {
				t.Fatalf("filtered rollup mismatch: keys=%+v models=%+v trend=%+v", keyRows, modelRows, trend)
			}
		})
	}
	for _, value := range []any{entries[0], keys[0], models[0], overview, all, tokens[0], UsageDailySummary{}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["usage_cost_micro_usd"]; !ok {
			t.Fatalf("missing usage_cost_micro_usd: %s", data)
		}
		if _, entry := value.(UsageEntry); !entry {
			if _, ok := fields["unpriced_count"]; !ok {
				t.Fatalf("missing unpriced_count: %s", data)
			}
		} else if _, ok := fields["usage_cost_status"]; !ok {
			t.Fatalf("missing usage_cost_status: %s", data)
		}
	}
	quotaAfter, err := st.ListModelTokenUsage(ctx, keyA.ID, keyA.ModelTokenLimits, now.UnixMilli())
	if err != nil || !reflect.DeepEqual(quotaBefore, quotaAfter) {
		t.Fatalf("quota changed: before=%+v after=%+v err=%v", quotaBefore, quotaAfter, err)
	}
	if after := financialState(); !reflect.DeepEqual(before, after) {
		t.Fatalf("financial state changed:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestUsageCostEmptySummaries(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	overview, err := st.UsageOverviewSummary(ctx)
	if err != nil || overview != (UsageOverviewSummary{}) {
		t.Fatalf("empty overview=%+v err=%v", overview, err)
	}
	summary, err := st.SummarizeUsageFiltered(ctx, UsageFilter{})
	if err != nil || summary != (UsageFilteredSummary{}) {
		t.Fatalf("empty filtered summary=%+v err=%v", summary, err)
	}
}
