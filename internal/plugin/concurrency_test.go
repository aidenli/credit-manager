package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// TestFallbackAttemptBypassesKeyConcurrency covers what a key's concurrency cap
// is for: bounding parallelism against the key's own accounts. An attempt the
// API-provider fallback serves must be admitted while that cap is already full,
// and must not appear in the key's active count.
func TestFallbackAttemptBypassesKeyConcurrency(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, material, err := svc.MintKeyWithPolicy(ctx, service.MintKeyRequest{
		CallerID: service.BootstrapCallerID, Label: "concurrency", QuotaMicroUSD: 1_000_000_000, MaxConcurrentRequests: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.BuildReservePlan(ctx, "gpt-5.6-luna", []byte(`{"model":"gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Hold the key's only slot the way an in-flight bound request would.
	if _, err := svc.Reserve(ctx, key, plan, "in-flight"); err != nil {
		t.Fatal(err)
	}

	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		var result any = map[string]any{}
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			result = pluginapi.HostModelStreamResponse{StreamID: "upstream", StatusCode: http.StatusOK}
		case pluginabi.MethodHostModelStreamRead:
			result = pluginapi.HostModelStreamReadResponse{Done: true}
		case pluginabi.MethodHostLog, pluginabi.MethodHostStreamEmit, pluginabi.MethodHostModelStreamClose:
		default:
			return nil, 0, fmt.Errorf("unexpected host method %s", method)
		}
		encoded, err := okEnvelope(result)
		return encoded, 0, err
	}
	body := []byte(`{"model":"gpt-5.6-luna","stream":true,"input":"hi","max_output_tokens":16}`)
	run := func(name, authID, provider string) error {
		return runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
			Model: "gpt-5.6-luna", SourceFormat: "openai-response", Format: "openai-response", Stream: true,
			AuthID: authID, AuthProvider: provider,
			Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}}, Payload: body,
		}}, "downstream-"+name, "")
	}

	if err := run("fallback", "openai-compatibility:deepseek:1a96cef1d695", "openai-compatible-deepseek"); err != nil {
		t.Fatalf("fallback attempt was rejected at the cap: %v", err)
	}
	if err := run("bound", "codex-43b33233-gdgpt3@163.com-prolite.json", "codex"); !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("bound attempt error = %v, want %v", err, store.ErrConcurrentLimit)
	}

	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("usage rows = %#v, err = %v", entries, err)
	}
	reservation, err := svc.Store().GetReservation(ctx, entries[0].ReservationID)
	if err != nil {
		t.Fatal(err)
	}
	if !reservation.Fallback {
		t.Fatalf("fallback attempt reservation is unmarked: %#v", reservation)
	}
	overview, err := svc.Store().GetKeyUsageOverview(ctx, key.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if overview.ActiveReservations != 1 {
		t.Fatalf("active reservations = %d, want 1 (only the in-flight bound request)", overview.ActiveReservations)
	}
}
