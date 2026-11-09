package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestUsageStatisticsViews(t *testing.T) {
	priceID := "reference-price"
	for _, tc := range []struct {
		name       string
		source     string
		price      *string
		statistics bool
		known      bool
	}{
		{"statistics-priced", "statistics", &priceID, true, true},
		{"statistics-unpriced", "statistics", nil, true, false},
		{"legacy-settled", "openai", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := store.UsageEntry{Source: tc.source, PricingRuleID: tc.price, EstimatedCostMicroUSD: 123}
			for _, view := range []map[string]any{usageView(entry), publicUsageView(entry)} {
				if view["statistics_only"] != tc.statistics || view["price_known"] != tc.known {
					t.Fatalf("statistics metadata = %#v", view)
				}
				if view["cost_micro_usd"] != money.MicroUSD(0) || view["estimated_cost_micro_usd"] != money.MicroUSD(123) {
					t.Fatalf("actual/reference cost = %#v", view)
				}
			}
			public := publicUsageView(entry)
			if _, ok := public["pricing_rule_id"]; ok {
				t.Fatal("public view exposes internal pricing ID")
			}
		})
	}
}

func TestUsageSummaryStatisticsStatus(t *testing.T) {
	svc := bindingsTestService(t)
	resp, err := usageSummary(context.Background(), svc, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("summary: status=%d err=%v body=%s", resp.StatusCode, err, resp.Body)
	}
	var body struct {
		Status map[string]uint64 `json:"statistics_status"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"queued", "accepted", "dropped", "write_failed", "unattributed", "written"} {
		if value, ok := body.Status[field]; !ok || value != 0 {
			t.Fatalf("new runtime status %s = %d, present=%t", field, value, ok)
		}
	}
}

func TestLookupIncludesStatisticsWithoutChangingBalance(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	key, material, err := svc.MintKey(ctx, "", "statistics lookup", 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().InsertUsageStatistics(ctx, []store.UsageEntry{
		{ID: "statistics-own", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "stats-model", Usage: money.TokenUsage{Input: 11, Output: 7}, EstimatedCostMicroUSD: 50},
		{ID: "statistics-other", PluginKeyID: "other-key", Model: "other-model", Usage: money.TokenUsage{Input: 99}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, recentOnly := range []string{"0", "1"} {
		t.Run("recent_only="+recentOnly, func(t *testing.T) {
			resp, err := lookupKey(ctx, svc, http.Header{"Authorization": {"Bearer " + material.Plaintext}}, map[string][]string{
				"recent_only": {recentOnly}, "plugin_key_id": {"other-key"},
			})
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("lookup: status=%d err=%v body=%s", resp.StatusCode, err, resp.Body)
			}
			var body struct {
				Key struct {
					Settled   int64 `json:"settled_spend_micro_usd"`
					Held      int64 `json:"held_amount_micro_usd"`
					Remaining int64 `json:"remaining_micro_usd"`
				} `json:"key"`
				Recent []struct {
					Model      string `json:"model"`
					Statistics bool   `json:"statistics_only"`
					Known      bool   `json:"price_known"`
					Cost       int64  `json:"cost_micro_usd"`
					Reference  int64  `json:"estimated_cost_micro_usd"`
				} `json:"recent_usage"`
				Pagination struct {
					Total int `json:"total"`
				} `json:"recent_pagination"`
				Summary struct {
					Count int `json:"request_count"`
					Cost  int `json:"cost_micro_usd"`
				} `json:"usage_summary"`
				Models []map[string]any `json:"by_model"`
				Trend  []map[string]any `json:"daily_trend"`
			}
			if err := json.Unmarshal(resp.Body, &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Recent) != 1 || body.Pagination.Total != 1 {
				t.Fatalf("lookup statistics pagination: %s", resp.Body)
			}
			row := body.Recent[0]
			if row.Model != "stats-model" || !row.Statistics || row.Known || row.Cost != 0 || row.Reference != 50 {
				t.Fatalf("lookup statistics row = %+v", row)
			}
			if recentOnly == "0" && (body.Key.Settled != 0 || body.Key.Held != 0 || body.Key.Remaining != 1000 || body.Summary.Count != 1 || body.Summary.Cost != 0 || len(body.Models) != 1 || len(body.Trend) != 1) {
				t.Fatalf("lookup balances/summaries: %s", resp.Body)
			}
			if strings.Contains(string(resp.Body), "statistics_status") {
				t.Fatal("readonly lookup exposes global runtime counters")
			}
		})
	}
}

func TestStatisticsUIExplainsCountsAndUsageCosts(t *testing.T) {
	for name, page := range map[string]string{"console": string(consolePage().Body), "lookup": string(lookupPage().Body)} {
		for _, text := range []string{"用量记录数", "不等于精确请求数", "statistics_only", "price_known", "usage_cost_micro_usd", "usage_cost_status", "unpriced_count", "未配置价格", "用量费用趋势", "不扣减余额或更新密钥已用额度", "Magpie 免费（0）"} {
			if !strings.Contains(page, text) {
				t.Errorf("%s missing %q", name, text)
			}
		}
	}
	lookup := string(lookupPage().Body)
	for _, text := range []string{"['coin',t('用量费用')", "trendChip('用量费用'", "name:t('用量费用')", "t('免费')"} {
		if !strings.Contains(lookup, text) {
			t.Errorf("lookup missing actual-charge label %q", text)
		}
	}
	console := string(consolePage().Body)
	for _, text := range []string{"usageStatisticsStatus", "statistics_status", "status.dropped || status.write_failed || status.unattributed", "统计记录可能不完整", "t('用量费用') + ' ' + currencyCode()", "<th>用量费用 ", "trendMetric('用量费用'", "t('免费')"} {
		if !strings.Contains(console, text) {
			t.Errorf("console missing status/drop warning %q", text)
		}
	}
}
