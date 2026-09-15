package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

type fakeAuthWarmupExecutor struct {
	calls  []AuthWarmupRequest
	result AuthWarmupResult
	err    error
}

func (f *fakeAuthWarmupExecutor) ExecuteAuthWarmup(_ context.Context, request AuthWarmupRequest) (AuthWarmupResult, error) {
	f.calls = append(f.calls, request)
	return f.result, f.err
}

func TestWarmupAuthQuotaIsClaimedAndSeparateFromCustomerBilling(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files:     []AuthQuotaFile{{ID: "auth-1", AuthIndex: "idx-1", Provider: "claude", Label: "ops"}},
		auth:      quotaJSON("claude"),
		responses: map[string]string{"oauth/usage": `{"five_hour":{"utilization":0.1,"resets_at":"2030-01-01T00:00:00Z"}}`},
	}
	s.SetAuthQuotaSource(src)
	executor := &fakeAuthWarmupExecutor{result: AuthWarmupResult{Model: "claude-haiku", Usage: money.TokenUsage{Input: 2, Output: 1}}}
	s.SetAuthWarmupExecutor(executor)

	item, err := s.WarmupAuthQuota(context.Background(), "claude", "auth-1", "idx-1", []string{"claude-haiku"})
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || executor.calls[0].Auth.AuthID != "auth-1" || len(executor.calls[0].Models) != 1 || executor.calls[0].Models[0] != "claude-haiku" {
		t.Fatalf("warmup calls = %#v", executor.calls)
	}
	if item.Warmup == nil || item.Warmup.Status != "succeeded" || item.Warmup.InputTokens != 2 || item.Warmup.OutputTokens != 1 {
		t.Fatalf("warmup item = %#v", item)
	}
	if _, err := s.WarmupAuthQuota(context.Background(), "claude", "auth-1", "idx-1", []string{"claude-haiku"}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("manual warmup did not execute twice: %d", len(executor.calls))
	}
	if usage, err := s.Store().GetAuthQuotaUsage(context.Background(), store.AuthQuotaUsageFilter{Provider: "claude", AuthID: "auth-1", AuthIndex: "idx-1", From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}); err != nil || usage.RequestCount != 0 {
		t.Fatalf("warmup must not create customer ledger usage: %#v, %v", usage, err)
	}
}

func TestWarmupScheduleKeysAreIndependent(t *testing.T) {
	schedules := []store.AuthWarmupSchedule{
		{ID: "codex", Auths: []store.AuthWarmupAuthTarget{{Provider: "codex", AuthID: "auth-codex", AuthIndex: "idx-codex"}}, Models: []string{"gpt-5.6-luna"}},
		{ID: "claude", Auths: []store.AuthWarmupAuthTarget{{Provider: "claude", AuthID: "auth-claude", AuthIndex: "idx-claude"}}, Models: []string{"claude-sonnet-4-6"}},
	}
	now := time.Date(2026, time.September, 15, 3, 0, 0, 0, time.UTC)
	if scheduledWarmupKey(schedules[0], now) == scheduledWarmupKey(schedules[1], now) {
		t.Fatal("different schedule tasks must have independent durable run keys")
	}
}

func TestManualWarmupRequiresModelsFromAuthFile(t *testing.T) {
	s := quotaService(t)
	if _, err := s.WarmupAuthQuota(context.Background(), "claude", "auth-1", "idx-1", nil); err == nil {
		t.Fatal("manual warmup without auth-file models must be rejected")
	}
}

func TestAuthQuotaWindowsChangedDetectsTinyUpstreamUsage(t *testing.T) {
	start := time.Date(2026, time.September, 15, 7, 0, 0, 0, time.UTC)
	reset := start.Add(7 * 24 * time.Hour)
	zero, tiny := 0.0, 0.0017
	before := []AuthQuotaWindow{{ID: "3p-weekly", Scope: "model_pool", ScopeID: "third-party", CycleStartAt: &start, ResetsAt: &reset, Used: &zero}}
	after := []AuthQuotaWindow{{ID: "3p-weekly", Scope: "model_pool", ScopeID: "third-party", CycleStartAt: &start, ResetsAt: &reset, Used: &tiny}}
	if !authQuotaWindowsChanged(before, after) {
		t.Fatal("tiny upstream percentage change must be observed")
	}
	if authQuotaWindowsChanged(after, after) {
		t.Fatal("identical quota windows must not be observed as changed")
	}
}

func TestAuthWarmupErrorCodeIdentifiesMissingAPIKey(t *testing.T) {
	if got := authWarmupErrorCode(errors.New("xai: HTTP 401: Missing API key")); got != "missing_api_key" {
		t.Fatalf("error code = %q", got)
	}
}

func TestAuthWarmupProviderErrorCodeIdentifiesOpaqueXAIFailure(t *testing.T) {
	if got := authWarmupProviderErrorCode("xai", errors.New("model execution failed")); got != "xai_auth_required" {
		t.Fatalf("XAI error code = %q", got)
	}
}

func TestWarmupStoreContextSurvivesSchedulerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := warmupStoreContext(ctx).Err(); err != nil {
		t.Fatalf("warmup store context must remain writable after cancellation: %v", err)
	}
}

func TestScheduledWarmupSkipsActiveWindowWithoutClaim(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files:     []AuthQuotaFile{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		auth:      quotaJSON("codex"),
		responses: map[string]string{"chatgpt.com": `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_at":4102444800}}}`},
	}
	s.SetAuthQuotaSource(src)
	executor := &fakeAuthWarmupExecutor{}
	s.SetAuthWarmupExecutor(executor)
	settings := store.DefaultAuthWarmupSettings()
	schedule := store.AuthWarmupSchedule{ID: "scheduled", Models: []string{"gpt-5.6-luna"}}
	if _, err := s.warmupAuthQuota(context.Background(), "", "codex", "auth-1", "idx-1", "scheduled:today", true, settings, schedule); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("active quota window must skip execution, calls=%d", len(executor.calls))
	}
	runs, err := s.Store().ListAuthWarmupRuns(context.Background(), "codex", "auth-1", 10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("active window skip must not claim schedule slot: %#v, %v", runs, err)
	}
}
