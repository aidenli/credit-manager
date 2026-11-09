package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
	"gopkg.in/yaml.v3"
)

func TestUsageStatisticsBoundedNonblockingQueue(t *testing.T) {
	w := &usageStatisticsWriter{queue: make(chan UsageStatistic, usageStatisticsCapacity)}
	svc := &Service{cfg: config.Config{AuthOnly: true}, statistics: w}
	record := UsageStatistic{Principal: "principal", Model: "test"}
	for i := 0; i < usageStatisticsCapacity; i++ {
		if !svc.ObserveUsageStatistic(record) {
			t.Fatalf("enqueue %d failed", i)
		}
	}
	if svc.ObserveUsageStatistic(record) {
		t.Fatal("overflow accepted")
	}
	record.Model = strings.Repeat("x", 513)
	if svc.ObserveUsageStatistic(record) {
		t.Fatal("oversized metadata accepted")
	}
	got := svc.UsageStatisticsStatus()
	if got.Queued != usageStatisticsCapacity || got.Accepted != usageStatisticsCapacity || got.Dropped != 2 {
		t.Fatalf("status = %+v", got)
	}
}

func TestUsageStatisticsDrainDedupAttributionAndNoBilling(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir, cfg.AuthOnly = t.TempDir(), true
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.Store().DeletePricingRule(ctx, BootstrapPricingRuleID); err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.MintKey(ctx, BootstrapCallerID, "stats", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{ID: "stats-price", MatchKind: store.MatchExact, Pattern: "stats-model", Enabled: true, Price: money.PricePerMTok{Input: 1_000_000, Output: 2_000_000}}); err != nil {
		t.Fatal(err)
	}
	record := UsageStatistic{EventID: "event-1", Principal: key.Principal, Model: "stats-model", Provider: "codex", AuthID: "account-id", RequestedAt: time.Now(), Usage: money.TokenUsage{Input: 10, Output: 20}}
	if !svc.ObserveUsageStatistic(record) || !svc.ObserveUsageStatistic(record) {
		t.Fatal("enqueue failed")
	}
	record.EventID, record.Principal = "wrong-principal", "not-a-customer"
	svc.ObserveUsageStatistic(record)
	record.EventID, record.Principal, record.Model = "unpriced", key.Principal, "no-price"
	svc.ObserveUsageStatistic(record)
	svc.statistics.close()
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%+v error=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.CostMicroUSD != 0 || entry.ReservationID != "" || entry.Source != "statistics" {
			t.Fatalf("financial row: %+v", entry)
		}
		if entry.Model == "stats-model" && (entry.EstimatedCostMicroUSD != 50 || entry.PricingRuleID == nil) {
			t.Fatalf("reference price: %+v", entry)
		}
		if entry.Model == "no-price" && entry.PricingRuleID != nil {
			t.Fatal("unknown price marked known")
		}
	}
	updated, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil || updated.SettledSpendMicroUSD != key.SettledSpendMicroUSD || updated.HeldAmountMicroUSD != key.HeldAmountMicroUSD {
		t.Fatalf("balance changed: %+v %v", updated, err)
	}
	for _, table := range []string{"reservations", "usage_ledger"} {
		var count int
		if err := svc.Store().DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	status := svc.UsageStatisticsStatus()
	if status.Written != 3 || status.Unattributed != 1 || status.Queued != 0 || status.WriteFailed != 0 {
		t.Fatalf("status=%+v", status)
	}
	if svc.ObserveUsageStatistic(record) {
		t.Fatal("closed writer accepted record")
	}
}

func TestUsageStatisticsReconfigureSharesWriterAndConcurrentClose(t *testing.T) {
	Shutdown()
	cfg := config.Default()
	cfg.DataDir, cfg.AuthOnly = t.TempDir(), true
	t.Cleanup(Shutdown)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	old := Current()
	if err := Configure(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	next := Current()
	if old == next || old.statistics != next.statistics {
		t.Fatal("reconfigure did not share writer")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				next.ObserveUsageStatistic(UsageStatistic{Principal: "unknown", Model: "model"})
			}
		}()
	}
	next.statistics.close()
	wg.Wait()
	next.statistics.close()
}
