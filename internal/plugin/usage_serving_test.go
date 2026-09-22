package plugin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
	"github.com/yuluo688/credit-manager/internal/usageparse"
)

// TestHandleUsageMarksAPIFallbackProvider covers the moment the marker is
// learned from the host's record alone: the plugin pinned a bound codex account,
// the host executed the request on the OpenAI-compatible provider, and the usage
// callback is the only place that says so. Without the marker the console would
// show the row as bound-account traffic.
func TestHandleUsageMarksAPIFallbackProvider(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "usage-serve", 1_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	plan, err := svc.BuildReservePlan(ctx, "gpt-5.6-luna", body)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Reserve(ctx, key, plan, "usage-serve")
	if err != nil {
		t.Fatal(err)
	}
	svc.TrackAuthCapture(reservation.ID, plan.Model)
	if err := svc.SettleFromUsage(ctx, reservation, plan, usageparse.Result{}, "openai", store.UsageMetrics{}); err != nil {
		t.Fatal(err)
	}

	raw := []byte(fmt.Sprintf(`{"Provider":"openai-compatible-deepseek","ExecutorType":"OpenAICompatExecutor",
		"Model":"gpt-5.6-luna","AuthID":"codex-43b33233-gdgpt3@163.com-prolite.json","RequestedAt":%q,
		"Detail":{"InputTokens":5,"OutputTokens":2}}`, time.Now().UTC().Format(time.RFC3339Nano)))
	if _, err := handleUsage(raw); err != nil {
		t.Fatalf("handle usage: %v", err)
	}

	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 1})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list usage = %#v, err = %v", entries, err)
	}
	entry := entries[0]
	if !entry.ServedAPI || entry.ServedProvider != "deepseek" {
		t.Fatalf("fallback marker = %#v", entry)
	}
	if entry.Auth.AuthID != "codex-43b33233-gdgpt3@163.com-prolite.json" {
		t.Fatalf("auth identity changed: %#v", entry.Auth)
	}
}
