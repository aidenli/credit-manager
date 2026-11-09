package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestUsageCostReportsWithoutFinancialDeduction(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	key, material, err := svc.MintKey(ctx, "", "usage cost", 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	price := "stored-price-snapshot"
	err = svc.Store().InsertUsageStatistics(ctx, []store.UsageEntry{
		{ID: "cost-oauth", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "oauth", PricingRuleID: &price, EstimatedCostMicroUSD: 125, Usage: money.TokenUsage{Input: 20, Output: 5}, Auth: store.AuthIdentity{Provider: "codex"}},
		{ID: "cost-magpie", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "magpie", PricingRuleID: &price, EstimatedCostMicroUSD: 999, Usage: money.TokenUsage{Input: 30}, ServedAPI: true, ServedProvider: "magpie"},
		{ID: "cost-unpriced", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "unknown", Usage: money.TokenUsage{Output: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := lookupKey(ctx, svc, http.Header{"Authorization": {"Bearer " + material.Plaintext}}, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup err=%v body=%s", err, resp.Body)
	}
	var body struct {
		Summary struct {
			Cost     int64 `json:"cost_micro_usd"`
			Usage    int64 `json:"usage_cost_micro_usd"`
			Unpriced int64 `json:"unpriced_count"`
		} `json:"usage_summary"`
		Recent []struct {
			Model  string `json:"model"`
			Usage  int64  `json:"usage_cost_micro_usd"`
			Status string `json:"usage_cost_status"`
			Known  bool   `json:"price_known"`
		} `json:"recent_usage"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Summary.Cost != 0 || body.Summary.Usage != 125 || body.Summary.Unpriced != 1 {
		t.Fatalf("summary=%+v", body.Summary)
	}
	statuses := map[string]string{"oauth": "priced", "magpie": "free", "unknown": "unpriced"}
	for _, row := range body.Recent {
		if row.Status != statuses[row.Model] {
			t.Fatalf("row=%+v", row)
		}
		if row.Model == "magpie" && (row.Usage != 0 || !row.Known) {
			t.Fatalf("free row=%+v", row)
		}
	}
	if len(body.Recent) != 3 {
		t.Fatalf("recent=%+v", body.Recent)
	}
	// Both private and public views expose exactly the same cost semantics.
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		a, b := usageView(entry), publicUsageView(entry)
		for _, field := range []string{"cost_micro_usd", "usage_cost_micro_usd", "usage_cost_status", "price_known"} {
			if a[field] != b[field] {
				t.Fatalf("%s differs: %v %v", field, a[field], b[field])
			}
		}
	}
	after, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SettledSpendMicroUSD != 0 || after.HeldAmountMicroUSD != 0 || after.RemainingMicroUSD() != 1000 {
		t.Fatalf("financial state changed: %+v", after)
	}
}
