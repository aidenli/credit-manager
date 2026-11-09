package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
)

func TestUsageStatisticsReportingAndFinancialIsolation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{
		Label:            "reporting key",
		ModelTokenLimits: []ModelTokenLimit{{Model: "test-model", Total: ModelPeriodTokenLimit{Tokens: 11}}},
	})
	reservation, err := st.Reserve(ctx, reserveRequest(key, "settled", 7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Settle(ctx, Settlement{LedgerID: "ledger", ReservationID: reservation.ID, Model: "test-model",
		Usage: money.TokenUsage{Input: 10}, CostMicroUSD: 7, EstimatedCostMicroUSD: 70}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	// Snapshot financial rows and enforcement totals, not just the zero cost of statistics.
	financialState := func() []any {
		t.Helper()
		caller, err := st.GetCaller(ctx, key.CallerID)
		if err != nil {
			t.Fatal(err)
		}
		storedKey, err := st.GetPluginKey(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		storedReservation, err := st.GetReservation(ctx, reservation.ID)
		if err != nil {
			t.Fatal(err)
		}
		ledger, err := st.GetUsage(ctx, "ledger")
		if err != nil {
			t.Fatal(err)
		}
		overview, err := st.GetKeyUsageOverview(ctx, key.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		quota, err := st.ListModelTokenUsage(ctx, key.ID, key.ModelTokenLimits, now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		var ledgerCount, reservationCount, auditCount int
		if err := st.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM usage_ledger),
			(SELECT COUNT(*) FROM reservations), (SELECT COUNT(*) FROM audit_events)`).Scan(&ledgerCount, &reservationCount, &auditCount); err != nil {
			t.Fatal(err)
		}
		return []any{caller, storedKey, storedReservation, ledger, overview, quota, ledgerCount, reservationCount, auditCount}
	}
	before := financialState()
	tier, result, thinking, pricing := "priority", "success", "high", "reference-only-rule"
	latency, duration, tps := 12*time.Millisecond, 2*time.Second, 10.0
	entry := UsageEntry{
		ID: "statistics-1", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "test-model", ExecutorType: "compat",
		Usage:                 money.TokenUsage{Input: 100, Output: 20, Reasoning: 3, Cached: 4, CacheRead: 5, CacheCreation: 6, ReportedTotal: 150},
		EstimatedCostMicroUSD: 99, PricingRuleID: &pricing, CreatedAt: now,
		// Even a misrouted caller cannot create financial state through this API.
		CostMicroUSD: 1234, ReservationID: reservation.ID, Source: "reserved_fallback",
		Auth:      AuthIdentity{AuthID: "auth", AuthIndex: "index", Name: "account", Label: "label", Provider: "codex", Type: "oauth", Email: "user@example.test", Path: "account.json"},
		Metrics:   UsageMetrics{Tier: &tier, Result: &result, ThinkingIntensity: &thinking, FirstTokenLatency: &latency, GenerationDuration: &duration, TokensPerSecond: &tps},
		ServedAPI: true, ServedProvider: "provider",
	}
	other := UsageEntry{ID: "statistics-2", CallerID: "unknown-caller", PluginKeyID: "unknown-key", Model: "cached-model",
		Usage: money.TokenUsage{Cached: 50}, EstimatedCostMicroUSD: 8}
	if err := st.InsertUsageStatistics(ctx, []UsageEntry{entry, other, entry}); err != nil {
		t.Fatal(err)
	}
	// Retrying the batch must not overwrite the first observation.
	retry := entry
	retry.Usage.Input = 999
	if err := st.InsertUsageStatistics(ctx, []UsageEntry{retry, other}); err != nil {
		t.Fatal(err)
	}
	if after := financialState(); !reflect.DeepEqual(before, after) {
		t.Fatalf("financial state changed:\nbefore=%+v\nafter=%+v", before, after)
	}
	entries, err := st.ListUsage(ctx, UsageFilter{LedgerID: entry.ID})
	if err != nil || len(entries) != 1 {
		t.Fatalf("statistics lookup=%+v err=%v", entries, err)
	}
	want := entry
	want.CostMicroUSD, want.ReservationID, want.Source, want.KeyLabel = 0, "", "statistics", key.Label
	want.UsageCostMicroUSD, want.UsageCostStatus = 99, "priced"
	if !reflect.DeepEqual(entries[0], want) {
		t.Fatalf("entry=%+v want=%+v", entries[0], want)
	}
	from, to := now.Add(-time.Minute), now.Add(time.Minute)
	usageCost, minTokens, maxTokens, served := money.MicroUSD(99), int64(150), int64(150), true
	filters := []UsageFilter{
		{Source: "statistics"}, {CallerID: key.CallerID, Source: "statistics"},
		{PluginKeyID: key.ID, Model: "test-model", Source: "statistics", AuthID: "auth", AuthIndex: "index", AuthProvider: "codex",
			From: &from, To: &to, MinCostMicroUSD: &usageCost, MaxCostMicroUSD: &usageCost, MinTokens: &minTokens, MaxTokens: &maxTokens, ServedAPI: &served},
		{Model: "cached-model"},
	}
	for i, filter := range filters {
		wantCount := int64(1)
		if i == 0 {
			wantCount = 2
		}
		count, err := st.CountUsage(ctx, filter)
		if err != nil || count != wantCount {
			t.Fatalf("filter %d count=%d err=%v", i, count, err)
		}
		rows, err := st.ListUsage(ctx, filter)
		if err != nil || int64(len(rows)) != wantCount {
			t.Fatalf("filter %d rows=%+v err=%v", i, rows, err)
		}
	}
	if rows, err := st.ListUsage(ctx, UsageFilter{Source: "statistics", Limit: 1, Offset: 1}); err != nil || len(rows) != 1 {
		t.Fatalf("pagination=%+v err=%v", rows, err)
	}
	overview, err := st.UsageOverviewSummary(ctx)
	if err != nil || overview.RequestCount != 3 || overview.TotalTokens != 210 || overview.EstimatedCostMicroUSD != 114 {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	all, err := st.SummarizeUsageFiltered(ctx, UsageFilter{})
	if err != nil || all.RequestCount != 3 || all.TotalTokens != 210 || all.CostMicroUSD != 7 || all.FallbackCount != 1 {
		t.Fatalf("all summary=%+v err=%v", all, err)
	}
	filter := UsageFilter{Source: "statistics", PluginKeyID: key.ID}
	summary, err := st.SummarizeUsageFiltered(ctx, filter)
	if err != nil || summary.RequestCount != 1 || summary.TotalTokens != 150 || summary.CostMicroUSD != 0 || summary.ReasoningTokens != 3 || summary.CacheCreationTokens != 6 {
		t.Fatalf("statistics summary=%+v err=%v", summary, err)
	}
	byKey, err := st.SummarizeUsageByKeyFiltered(ctx, filter)
	if err != nil || len(byKey) != 1 || byKey[0].Label != key.Label || byKey[0].RequestCount != 1 || byKey[0].InputTokens != 100 || byKey[0].CostMicroUSD != 0 {
		t.Fatalf("byKey=%+v err=%v", byKey, err)
	}
	byModel, err := st.SummarizeUsageByModelFiltered(ctx, filter)
	if err != nil || len(byModel) != 1 || byModel[0].TotalTokens != 150 || byModel[0].CacheReadTokens != 5 || byModel[0].AvgTokensPerSecond == nil || *byModel[0].AvgTokensPerSecond != tps {
		t.Fatalf("byModel=%+v err=%v", byModel, err)
	}
	trend, err := st.SummarizeUsageTrendFiltered(ctx, filter, "day")
	if err != nil || len(trend) != 1 || trend[0].InputTokens != 100 || trend[0].OutputTokens != 20 || trend[0].CostMicroUSD != 0 {
		t.Fatalf("trend=%+v err=%v", trend, err)
	}
	tokens, err := st.UsageTokenTrend(ctx, 1)
	if err != nil || len(tokens) != 1 || tokens[0].TotalTokens != 210 {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
	if _, err := st.GetUsage(ctx, entry.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("GetUsage statistics: %v", err)
	}
	if err := st.UpdateUsageDetail(ctx, entry.ID, money.TokenUsage{Input: 1}, 100); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reprice statistics: %v", err)
	}
	if _, found, err := st.FindRecentFallback(ctx, nil, time.Minute); err != nil || found {
		t.Fatalf("fallback found=%t err=%v", found, err)
	}
	if after := financialState(); !reflect.DeepEqual(before, after) {
		t.Fatal("statistics repricing changed financial state")
	}
	// Only the ten ledger tokens count against the eleven-token cap.
	if _, err := st.Reserve(ctx, reserveRequest(key, "after-statistics", 1)); err != nil {
		t.Fatalf("statistics affected enforcement: %v", err)
	}
	if _, err := st.Reserve(ctx, reserveRequest(key, "over-cap", 1)); !errors.Is(err, ErrTotalTokenLimitExceeded) {
		t.Fatalf("ledger enforcement lost: %v", err)
	}
}

func TestUsageStatisticsBatchRollbackAndConstraints(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	valid := UsageEntry{ID: "valid", Model: "model"}
	for _, invalid := range []UsageEntry{
		{Model: "model"}, {ID: "bad"}, {ID: "bad", Model: "model", EstimatedCostMicroUSD: -1},
		{ID: "bad", Model: "model", Usage: money.TokenUsage{Input: -1}},
	} {
		if err := st.InsertUsageStatistics(ctx, []UsageEntry{valid, invalid}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid entry err=%v", err)
		}
		if count, err := st.CountUsage(ctx, UsageFilter{}); err != nil || count != 0 {
			t.Fatalf("partial batch count=%d err=%v", count, err)
		}
	}
	if err := st.InsertUsageStatistics(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertUsageStatistics(ctx, []UsageEntry{valid}); err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListUsage(ctx, UsageFilter{})
	if err != nil || len(entries) != 1 || time.Since(entries[0].CreatedAt) > time.Minute {
		t.Fatalf("default timestamp=%+v err=%v", entries, err)
	}
	for _, statement := range []string{
		`UPDATE usage_statistics SET cost_micro_usd = 1`,
		`UPDATE usage_statistics SET reservation_id = 'reservation'`,
		`UPDATE usage_statistics SET source = 'reserved_fallback'`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("financial invariant accepted: %s", statement)
		}
	}
	// The display view itself is read-only.
	if _, err := st.db.ExecContext(ctx, `UPDATE usage_reporting SET cost_micro_usd = 1`); err == nil {
		t.Fatal("reporting view is writable")
	}
	if err := Migrate(ctx, st.db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}

func TestUsageStatisticsIDCollisionCannotShadowLedger(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{})
	reservation, err := st.Reserve(ctx, reserveRequest(key, "collision", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Settle(ctx, Settlement{LedgerID: "shared-id", ReservationID: reservation.ID, Model: "test-model", Source: "reserved_fallback", CostMicroUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertUsageStatistics(ctx, []UsageEntry{{ID: "shared-id", Model: "test-model", Usage: money.TokenUsage{Input: 100}, CreatedAt: time.Now().Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	entry, err := st.GetUsage(ctx, "shared-id")
	if err != nil || entry.ReservationID != reservation.ID || entry.Source != "reserved_fallback" {
		t.Fatalf("ledger shadowed: %+v err=%v", entry, err)
	}
	fallback, found, err := st.FindRecentFallback(ctx, []string{"test-model"}, time.Minute)
	if err != nil || !found || fallback.ReservationID != reservation.ID {
		t.Fatalf("fallback shadowed: %+v found=%t err=%v", fallback, found, err)
	}
	if err := st.UpdateUsageDetail(ctx, "shared-id", money.TokenUsage{Input: 2}, 2); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListUsage(ctx, UsageFilter{LedgerID: "shared-id", Source: "statistics"})
	if err != nil || len(rows) != 1 || rows[0].Usage.Input != 100 || rows[0].CostMicroUSD != 0 {
		t.Fatalf("statistics mutated: %+v err=%v", rows, err)
	}
}
