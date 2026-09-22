package management

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
	"github.com/yuluo688/credit-manager/internal/usageparse"
)

// TestUsageFallbackMarkerIsExposedAndFilterable pins the console contract for
// fallback traffic: the ledger marker reaches the usage view, the served_api
// query parameter filters both the list and the rollups, and a malformed value
// is rejected instead of silently ignored.
func TestUsageFallbackMarkerIsExposedAndFilterable(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "usage-fallback", 1_000_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed := usageparse.Result{Found: true, Usage: money.TokenUsage{Input: 10, Output: 5}, Source: "test"}

	settle := func(name string, serving service.ServingInfo) {
		t.Helper()
		plan, err := svc.BuildReservePlan(ctx, "gpt-5.6-luna", []byte(`{"model":"gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := svc.Reserve(ctx, key, plan, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.SettleFromUsage(ctx, reservation, plan, parsed, "openai", store.UsageMetrics{}); err != nil {
			t.Fatal(err)
		}
		entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 1})
		if err != nil || len(entries) != 1 {
			t.Fatalf("list usage = %#v, err = %v", entries, err)
		}
		if err := svc.RecordServing(ctx, entries[0].ID, serving); err != nil {
			t.Fatal(err)
		}
	}

	settle("bound", service.ServingInfo{})
	settle("fallback", service.ServingInfo{API: true, Provider: "deepseek"})

	resp, err := listUsage(ctx, svc, map[string][]string{"served_api": {"1"}, "plugin_key_id": {key.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		t.Fatalf("decode page: %v body=%s", err, resp.Body)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("fallback page = %#v", page)
	}
	item := page.Items[0]
	if item["served_api"] != true || item["served_provider"] != "deepseek" {
		t.Fatalf("served fields = %#v", item)
	}

	summary, err := usageSummary(ctx, svc, map[string][]string{"plugin_key_id": {key.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var rollup struct {
		ByKey []struct {
			FallbackCount int64 `json:"fallback_count"`
			RequestCount  int64 `json:"request_count"`
		} `json:"by_key"`
	}
	if err := json.Unmarshal(summary.Body, &rollup); err != nil {
		t.Fatalf("decode summary: %v body=%s", err, summary.Body)
	}
	if len(rollup.ByKey) != 1 || rollup.ByKey[0].FallbackCount != 1 || rollup.ByKey[0].RequestCount != 2 {
		t.Fatalf("by key rollup = %#v", rollup.ByKey)
	}

	bad, err := listUsage(ctx, svc, map[string][]string{"served_api": {"maybe"}})
	if err != nil {
		t.Fatal(err)
	}
	if bad.StatusCode != 400 {
		t.Fatalf("malformed served_api status = %d, want 400", bad.StatusCode)
	}
}

// TestReleasedUsageListsAttemptsWithReasons covers the released-attempt list: a
// reservation that was released instead of settled must appear with its release
// reason split into code and upstream detail, because that text is the only
// record of what the client was shown when the attempt was the last one.
func TestReleasedUsageListsAttemptsWithReasons(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "released", 1_000_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.BuildReservePlan(ctx, "gpt-5.6-luna", []byte(`{"model":"gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Reserve(ctx, key, plan, "released-attempt")
	if err != nil {
		t.Fatal(err)
	}
	upstream := `host_call_failed: {"error":{"message":"unknown provider for model glm-5.3-flash"}}`
	if err := svc.Release(ctx, reservation.ID, "upstream_stream_error: "+upstream); err != nil {
		t.Fatal(err)
	}

	resp, err := listReleasedUsage(ctx, svc, map[string][]string{"plugin_key_id": {key.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		t.Fatalf("decode released page: %v body=%s", err, resp.Body)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("released page = %#v", page)
	}
	item := page.Items[0]
	if item["reason_code"] != "upstream_stream_error" || item["model"] != "gpt-5.6-luna" {
		t.Fatalf("released item = %#v", item)
	}
	if reason, _ := item["reason"].(string); !strings.Contains(reason, "glm-5.3-flash") {
		t.Fatalf("release reason lost the upstream text: %#v", item)
	}
	if item["key_label"] != "released" {
		t.Fatalf("released key label = %#v", item["key_label"])
	}

	other, err := listReleasedUsage(ctx, svc, map[string][]string{"model": {"other-model"}})
	if err != nil {
		t.Fatal(err)
	}
	var empty struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(other.Body, &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Total != 0 {
		t.Fatalf("model filter ignored: %#v", empty)
	}
}
