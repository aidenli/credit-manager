package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestDecodeUsageStatisticWhitelist(t *testing.T) {
	// Wrong types for ignored fields prove they are not decoded or retained.
	raw := []byte(`{"RequestID":"event","APIKey":"principal","Model":"model","AuthID":"account","Failed":true,"Latency":1000000000,"TTFT":100000000,"Source":{"secret":true},"Failure":{"Body":{"not":"a string"}},"ResponseHeaders":42,"Body":"NOT BASE64","Detail":{"TokenBreakdown":{"Input":{"TotalTokens":10,"CacheReadTokens":4},"Output":{"TotalTokens":5,"ReasoningTokens":2},"TotalTokens":15}}}`)
	record, ok := decodeUsageStatistic(raw)
	if !ok || record.Principal != "principal" || !record.Failed || record.Usage.Input != 10 || record.Usage.Output != 5 || record.Usage.CacheRead != 4 || record.Usage.ReportedTotal != 15 {
		t.Fatalf("record=%+v ok=%v", record, ok)
	}
	if _, ok := decodeUsageStatistic([]byte(strings.Repeat(" ", maxStatisticsEventBytes+1))); ok {
		t.Fatal("oversized event accepted")
	}
	if _, ok := decodeUsageStatistic([]byte(`{"Detail":{"InputTokens":"bad"}}`)); ok {
		t.Fatal("invalid token accepted")
	}
}

func TestAuthOnlyRejectionsVisibleWithoutFinancialRecords(t *testing.T) {
	ctx := context.Background()
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir, cfg.AuthOnly = t.TempDir(), true
	svc, err := service.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(service.Shutdown)
	key, plain, err := svc.MintKey(ctx, service.BootstrapCallerID, "rejection-test", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	cap := int64(1)
	if _, err := svc.Store().UpdatePluginKeyPolicy(ctx, store.PluginKeyPolicyUpdate{ID: key.ID, MaxConcurrentRequests: &cap}); err != nil {
		t.Fatal(err)
	}
	call := func(id, model string) pluginapi.RequestInterceptResponse {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"RequestID": id, "RequestedModel": model, "Model": "upstream-model",
			"Headers": http.Header{"Authorization": {"Bearer " + plain.Plaintext}},
			"Body":    "SECRET BODY NOT BASE64", "Metadata": map[string]any{"secret": "PRIVATE METADATA"},
		})
		response, err := interceptRequestAfterAuth(raw)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		var decoded pluginapi.RequestInterceptResponse
		if err := json.Unmarshal(response, &env); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(env.Result, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	activeID := "active-" + key.ID
	t.Cleanup(func() { releaseKeySlot(activeID) })
	if resp := call(activeID, "gpt-6-luna"); resp.Terminate {
		t.Fatalf("first request rejected: %+v", resp)
	}
	for range 2 { // Host callback retries must not duplicate the rejection row.
		if resp := call("rejected-"+key.ID, "gpt-6-luna"); !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(resp.ResponseBody), store.ErrConcurrentLimit.Error()) {
			t.Fatalf("concurrency response changed: %+v", resp)
		}
	}
	items, total, err := svc.Store().ListReleasedReservations(ctx, store.ReleasedReservationFilter{PluginKeyID: key.ID, IncludeRejected: true})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("rejections=%+v total=%d err=%v", items, total, err)
	}
	item := items[0]
	if item.Kind != "rejected" || item.RequestID != "rejected-"+key.ID || item.StatusCode != 429 || item.Model != "gpt-6-luna" || item.KeyLabel != key.Label || item.Reason != "key_concurrency_limit: "+store.ErrConcurrentLimit.Error() || item.ReleasedAt != nil || item.HeldMicroUSD != 0 {
		t.Fatalf("rejection=%+v", item)
	}
	serialized, _ := json.Marshal(items)
	for _, secret := range []string{plain.Plaintext, "SECRET BODY", "PRIVATE METADATA", "Bearer"} {
		if strings.Contains(string(serialized), secret) {
			t.Fatalf("rejection retained secret %q", secret)
		}
	}
	for _, table := range []string{"reservations", "usage_ledger", "usage_statistics"} {
		var count int
		if err := svc.Store().DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err := completeInterceptedRequest([]byte(`{"RequestID":"` + activeID + `"}`)); err != nil {
		t.Fatal(err)
	}
	afterID := "after-" + key.ID
	t.Cleanup(func() { releaseKeySlot(afterID) })
	if resp := call(afterID, "gpt-6-luna"); resp.Terminate {
		t.Fatalf("slot not released: %+v", resp)
	}
	models := []string{"allowed-only"}
	if _, err := svc.Store().UpdatePluginKeyPolicy(ctx, store.PluginKeyPolicyUpdate{ID: key.ID, AllowedModels: &models}); err != nil {
		t.Fatal(err)
	}
	if resp := call("model-denied-"+key.ID, "gpt-6-luna"); !resp.Terminate || resp.StatusCode != 403 {
		t.Fatalf("model response=%+v", resp)
	}
	items, total, err = svc.Store().ListReleasedReservations(ctx, store.ReleasedReservationFilter{PluginKeyID: key.ID, IncludeRejected: true})
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("policy rejection missing: %+v total=%d err=%v", items, total, err)
	}
	// Persistence failure is best effort: it must not turn a denial into success.
	if _, err := svc.Store().DB().Exec(`CREATE TRIGGER fail_rejection_insert BEFORE INSERT ON request_rejections BEGIN SELECT RAISE(ABORT, 'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if resp := call("write-failure-"+key.ID, "gpt-6-luna"); !resp.Terminate || resp.StatusCode != 403 {
		t.Fatalf("write failure changed rejection: %+v", resp)
	}
}

func BenchmarkDecodeUsageStatisticIgnoredBody(b *testing.B) {
	for _, size := range []int{16, 512 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			raw := []byte(`{"APIKey":"principal","Model":"model","Failure":{"Body":"` + strings.Repeat("x", size) + `"}}`)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := decodeUsageStatistic(raw); !ok {
					b.Fatal("decode failed")
				}
			}
		})
	}
}

func TestAuthOnlyStatisticsNoBodyNoImageHoldAndPermissions(t *testing.T) {
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir, cfg.AuthOnly = t.TempDir(), true
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(service.Shutdown)
	reg := pluginRegistration(supportedRPCSchemaVersion)
	if reg.Capabilities.Executor || reg.Capabilities.ModelRouter || reg.Capabilities.ResponseInterceptor || !reg.Capabilities.FrontendAuthProviderOmitBody || !reg.Capabilities.RequestInterceptorOmitBodyAfter || !reg.Capabilities.UsagePlugin {
		t.Fatalf("registration=%+v", reg.Capabilities)
	}
	key, plain, err := svc.MintKey(context.Background(), service.BootstrapCallerID, "stats-test", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"RequestID": "statistics-image-request", "SourceFormat": "openai-image", "Model": "gpt-image-1", "RequestedModel": "gpt-image-1", "Headers": http.Header{"Authorization": []string{"Bearer " + plain.Plaintext}}, "Body": "NOT BASE64", "Metadata": map[string]any{"body": strings.Repeat("x", 1000)}}
	call := func() pluginapi.RequestInterceptResponse {
		t.Helper()
		raw, _ := json.Marshal(request)
		response, err := interceptRequestAfterAuth(raw)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(response, &env); err != nil {
			t.Fatal(err)
		}
		var decoded pluginapi.RequestInterceptResponse
		if err := json.Unmarshal(env.Result, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	if resp := call(); resp.Terminate {
		t.Fatalf("zero-credit image request rejected: %+v", resp)
	}
	var count int
	if err := svc.Store().DB().QueryRow("SELECT COUNT(*) FROM reservations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("image hold=%d err=%v", count, err)
	}
	if _, err := completeInterceptedRequest([]byte(`{"RequestID":"statistics-image-request","Error":{"body":"ignored"},"Metadata":{"body":"ignored"}}`)); err != nil {
		t.Fatal(err)
	}
	models := []string{"allowed-only"}
	if _, err := svc.Store().UpdatePluginKeyPolicy(context.Background(), store.PluginKeyPolicyUpdate{ID: key.ID, AllowedModels: &models}); err != nil {
		t.Fatal(err)
	}
	if resp := call(); !resp.Terminate || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("model allowlist not enforced: %+v", resp)
	}
	if _, err := interceptModelDirectoryResponse([]byte(`{"Body":"NOT BASE64"}`)); err != nil {
		t.Fatalf("response body parsed: %v", err)
	}
	if _, err := handleUsage([]byte(`{"Model":"model","APIKey":"unknown","Source":{"secret":true}}`)); err != nil {
		t.Fatal(err)
	}
}
