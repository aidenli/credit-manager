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

// A warmup must request exactly one model: firing every model at every account is
// what turns a warmup batch into a burst of upstream 502s.
func TestWarmupAuthQuotaRequestsOnlyOneModel(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files: []AuthQuotaFile{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex", Label: "ops"}},
		auth:  quotaJSON("codex"),
		responses: map[string]string{
			"chatgpt.com": `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":3600,"reset_at":4102444800}}}`,
		},
	}
	s.SetAuthQuotaSource(src)
	executor := &fakeAuthWarmupExecutor{result: AuthWarmupResult{Model: "gpt-5.6-sol", Usage: money.TokenUsage{Input: 2, Output: 1}}}
	s.SetAuthWarmupExecutor(executor)

	// The account's model list arrives sorted, so the preferred model is not first.
	offered := []string{"codex-auto-review", "gpt-5.5", "gpt-5.6-sol", "gpt-image-2.5"}
	if _, err := s.WarmupAuthQuota(context.Background(), "codex", "auth-1", "idx-1", offered); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("warmup calls = %d, want 1", len(executor.calls))
	}
	got := executor.calls[0].Models
	if len(got) != 1 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("warmup models = %#v, want exactly gpt-5.6-sol", got)
	}
}

func TestPickAuthWarmupModel(t *testing.T) {
	cases := []struct {
		name   string
		models []string
		want   string
	}{
		{"preferred wins over order", []string{"codex-auto-review", "gpt-5.5", "gpt-5.6-sol"}, "gpt-5.6-sol"},
		{"preferred is case insensitive", []string{"GPT-5.6-SOL"}, "GPT-5.6-SOL"},
		{"falls back to first warmable", []string{"codex-auto-review", "gpt-5.5"}, "codex-auto-review"},
		{"skips image models", []string{"gpt-image-2.5", "gpt-5.5"}, "gpt-5.5"},
		{"only image models", []string{"gpt-image-2.5-sunburst"}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickAuthWarmupModel(tc.models); got != tc.want {
				t.Fatalf("pickAuthWarmupModel(%#v) = %q, want %q", tc.models, got, tc.want)
			}
		})
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

func TestScheduledWarmupKeyFollowsClockNotDay(t *testing.T) {
	now := time.Date(2026, time.September, 16, 11, 49, 0, 0, time.UTC)
	morning := store.AuthWarmupSchedule{ID: "same", Timezone: "UTC", WarmupAt: "11:11"}
	later := store.AuthWarmupSchedule{ID: "same", Timezone: "UTC", WarmupAt: "11:49"}
	if scheduledWarmupKey(morning, now) == scheduledWarmupKey(later, now) {
		t.Fatal("changing warmup time must start a new run instead of reusing the day")
	}
}

func TestAuthWarmupIsDueHonorsWeeklyWeekday(t *testing.T) {
	settings := store.DefaultAuthWarmupSettings()
	settings.StableJitterSeconds = 0
	schedule := store.AuthWarmupSchedule{ID: "weekly", Frequency: store.AuthWarmupFrequencyWeekly, Weekdays: []int{int(time.Monday)}, Timezone: "UTC", WarmupAt: "11:00"}
	file := AuthQuotaFile{ID: "auth-1"}
	monday := time.Date(2026, time.September, 14, 11, 1, 0, 0, time.UTC)
	tuesday := time.Date(2026, time.September, 15, 11, 1, 0, 0, time.UTC)
	if !authWarmupIsDue(settings, schedule, file, monday) {
		t.Fatal("weekly Monday must be due on Monday")
	}
	if authWarmupIsDue(settings, schedule, file, tuesday) {
		t.Fatal("weekly Monday must not run on Tuesday")
	}
	schedule.Weekdays = []int{int(time.Monday), int(time.Wednesday)}
	wednesday := time.Date(2026, time.September, 16, 11, 1, 0, 0, time.UTC)
	if !authWarmupIsDue(settings, schedule, file, monday) || !authWarmupIsDue(settings, schedule, file, wednesday) {
		t.Fatal("weekly multi-select must run on each selected weekday")
	}
	if authWarmupIsDue(settings, schedule, file, tuesday) {
		t.Fatal("weekly multi-select must not run on an unselected weekday")
	}
	schedule.Frequency = store.AuthWarmupFrequencyDaily
	if !authWarmupIsDue(settings, schedule, file, tuesday) {
		t.Fatal("daily schedule must still run on Tuesday")
	}
	schedule.Frequency = store.AuthWarmupFrequencyOnce
	schedule.WarmupOn = "2026-09-15"
	if !authWarmupIsDue(settings, schedule, file, tuesday) {
		t.Fatal("once schedule must run on the selected date")
	}
	schedule.WarmupOn = "2026-09-16"
	if authWarmupIsDue(settings, schedule, file, tuesday) {
		t.Fatal("once schedule must not run on a different date")
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

// A warmup failure must record the HTTP status it was refused with, otherwise a
// host-side rejection and a broken account are indistinguishable in the run table.
func TestAuthWarmupErrorCodeRecordsUpstreamStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"service unavailable", &AuthWarmupUpstreamStatusError{Status: 503, Message: "warmup request returned status 503"}, "status_503"},
		{"bad request", &AuthWarmupUpstreamStatusError{Status: 400}, "status_400"},
		{"image only model", ErrAuthWarmupModelUnavailable, "unsupported_model"},
		{"unauthorized keeps its own code", &AuthWarmupUpstreamStatusError{Status: 401, Message: "warmup request unauthorized"}, "unauthorized"},
		{"rate limited keeps its own code", &AuthWarmupUpstreamStatusError{Status: 429, Message: "warmup request rate limited"}, "rate_limited"},
		{"unknown error", errors.New("model execution failed"), "execution_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authWarmupErrorCode(tc.err); got != tc.want {
				t.Fatalf("error code = %q, want %q", got, tc.want)
			}
		})
	}
}

// A status-bearing error still renders the actionable message, not the number.
func TestAuthWarmupUpstreamStatusErrorMessage(t *testing.T) {
	err := &AuthWarmupUpstreamStatusError{Status: 503, Message: "warmup request returned status 503"}
	if err.Error() != "warmup request returned status 503" {
		t.Fatalf("message = %q", err.Error())
	}
	bare := &AuthWarmupUpstreamStatusError{Status: 502}
	if bare.Error() != "warmup request returned status 502" {
		t.Fatalf("bare message = %q", bare.Error())
	}
}

func TestWarmupStoreContextSurvivesSchedulerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := warmupStoreContext(ctx).Err(); err != nil {
		t.Fatalf("warmup store context must remain writable after cancellation: %v", err)
	}
}

func TestScheduledWarmupRunsWithActiveShortWindow(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files:     []AuthQuotaFile{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		auth:      quotaJSON("codex"),
		responses: map[string]string{"chatgpt.com": `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_at":4102444800}}}`},
	}
	s.SetAuthQuotaSource(src)
	executor := &fakeAuthWarmupExecutor{result: AuthWarmupResult{Model: "gpt-5.6-luna", Usage: money.TokenUsage{Input: 3, Output: 1}}}
	s.SetAuthWarmupExecutor(executor)
	schedule := store.AuthWarmupSchedule{ID: "scheduled", Models: []string{"gpt-5.6-luna"}}
	item, err := s.warmupAuthQuota(context.Background(), "", "codex", "auth-1", "idx-1", "scheduled:today", schedule)
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || executor.calls[0].Auth.AuthID != "auth-1" {
		t.Fatalf("active short window must not skip scheduled warmup, calls=%#v", executor.calls)
	}
	if item.Warmup == nil || item.Warmup.Status != "succeeded" {
		t.Fatalf("warmup item = %#v", item)
	}
	runs, err := s.Store().ListAuthWarmupRuns(context.Background(), "codex", "auth-1", 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatalf("scheduled warmup must claim the slot: %#v, %v", runs, err)
	}
}

func TestOnceAuthWarmupDisablesSchedule(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files:     []AuthQuotaFile{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		auth:      quotaJSON("codex"),
		responses: map[string]string{"chatgpt.com": `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":3600,"reset_at":4102444800}}}`},
	}
	s.SetAuthQuotaSource(src)
	s.SetAuthWarmupExecutor(&fakeAuthWarmupExecutor{result: AuthWarmupResult{Model: "gpt-5.6-luna", Usage: money.TokenUsage{Input: 1, Output: 1}}})
	schedule := store.AuthWarmupSchedule{
		ID: "once-task", Enabled: true, Frequency: store.AuthWarmupFrequencyOnce, Timezone: "UTC", WarmupAt: "11:00", WarmupOn: "2026-09-16",
		Auths: []store.AuthWarmupAuthTarget{{Provider: "codex", AuthID: "auth-1", AuthIndex: "idx-1"}}, Models: []string{"gpt-5.6-luna"},
	}
	settings := store.DefaultAuthWarmupSettings()
	settings.Schedules = []store.AuthWarmupSchedule{schedule}
	if _, err := s.Store().UpsertAuthWarmupSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := s.warmupAuthQuota(context.Background(), "", "codex", "auth-1", "idx-1", "scheduled:once", schedule); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Store().GetAuthWarmupSettings(context.Background())
	if err != nil || len(loaded.Schedules) != 1 || loaded.Schedules[0].Enabled {
		t.Fatalf("once schedule must disable after execution: %#v, %v", loaded.Schedules, err)
	}
}

type blockingAuthWarmupExecutor struct {
	started chan string
	release chan struct{}
	result  AuthWarmupResult
}

func (e *blockingAuthWarmupExecutor) ExecuteAuthWarmup(_ context.Context, request AuthWarmupRequest) (AuthWarmupResult, error) {
	e.started <- request.Auth.AuthID
	<-e.release
	return e.result, nil
}

func TestScheduledWarmupRunsDifferentAuthsConcurrently(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files: []AuthQuotaFile{
			{ID: "auth-a", AuthIndex: "idx-a", Provider: "claude"},
			{ID: "auth-b", AuthIndex: "idx-b", Provider: "claude"},
		},
		auth:      quotaJSON("claude"),
		responses: map[string]string{"oauth/usage": `{"five_hour":{"utilization":0.1,"resets_at":"2030-01-01T00:00:00Z"}}`},
	}
	s.SetAuthQuotaSource(src)
	started := make(chan string, 2)
	release := make(chan struct{})
	s.SetAuthWarmupExecutor(&blockingAuthWarmupExecutor{
		started: started,
		release: release,
		result:  AuthWarmupResult{Model: "claude-haiku", Usage: money.TokenUsage{Input: 1, Output: 1}},
	})
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 16, 11, 10, 30, 0, location)
	settings := store.AuthWarmupSettings{
		MaxParallel:         1,
		StableJitterSeconds: 0,
		Schedules: []store.AuthWarmupSchedule{
			{ID: "task-a", Enabled: true, Timezone: "Asia/Shanghai", WarmupAt: "11:10", Auths: []store.AuthWarmupAuthTarget{{Provider: "claude", AuthID: "auth-a", AuthIndex: "idx-a"}}, Models: []string{"claude-haiku"}},
			{ID: "task-b", Enabled: true, Timezone: "Asia/Shanghai", WarmupAt: "11:10", Auths: []store.AuthWarmupAuthTarget{{Provider: "claude", AuthID: "auth-b", AuthIndex: "idx-b"}}, Models: []string{"claude-haiku"}},
		},
	}
	if _, err := s.Store().UpsertAuthWarmupSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	s.runScheduledAuthWarmups(context.Background(), now)
	seen := map[string]struct{}{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-started:
			seen[id] = struct{}{}
		case <-deadline:
			t.Fatalf("different credentials must start concurrently with per-auth parallel=1: %#v", seen)
		}
	}
	close(release)
	waitAuthWarmupFinished(t, s, "claude", "auth-a")
	waitAuthWarmupFinished(t, s, "claude", "auth-b")
}

func TestScheduledWarmupCapsSameAuthParallel(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files:     []AuthQuotaFile{{ID: "auth-a", AuthIndex: "idx-a", Provider: "claude"}},
		auth:      quotaJSON("claude"),
		responses: map[string]string{"oauth/usage": `{"five_hour":{"utilization":0.1,"resets_at":"2030-01-01T00:00:00Z"}}`},
	}
	s.SetAuthQuotaSource(src)
	started := make(chan string, 2)
	release := make(chan struct{})
	s.SetAuthWarmupExecutor(&blockingAuthWarmupExecutor{
		started: started,
		release: release,
		result:  AuthWarmupResult{Model: "claude-haiku", Usage: money.TokenUsage{Input: 1, Output: 1}},
	})
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 16, 11, 10, 30, 0, location)
	settings := store.AuthWarmupSettings{
		MaxParallel:         1,
		StableJitterSeconds: 0,
		Schedules: []store.AuthWarmupSchedule{
			{ID: "task-a", Enabled: true, Timezone: "Asia/Shanghai", WarmupAt: "11:10", Auths: []store.AuthWarmupAuthTarget{{Provider: "claude", AuthID: "auth-a", AuthIndex: "idx-a"}}, Models: []string{"claude-haiku"}},
			{ID: "task-b", Enabled: true, Timezone: "Asia/Shanghai", WarmupAt: "11:10", Auths: []store.AuthWarmupAuthTarget{{Provider: "claude", AuthID: "auth-a", AuthIndex: "idx-a"}}, Models: []string{"claude-haiku"}},
		},
	}
	if _, err := s.Store().UpsertAuthWarmupSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	s.runScheduledAuthWarmups(context.Background(), now)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first same-auth warmup did not start")
	}
	select {
	case extra := <-started:
		close(release)
		t.Fatalf("same credential exceeded max parallel: %s", extra)
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	waitAuthWarmupFinished(t, s, "claude", "auth-a")
}

func waitAuthWarmupFinished(t *testing.T, s *Service, provider, authID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.Store().ListAuthWarmupRuns(context.Background(), provider, authID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) > 0 && runs[0].Status != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("warmup for %s/%s did not finish", provider, authID)
}
