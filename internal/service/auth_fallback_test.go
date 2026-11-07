package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

// Real-world shapes from a CLIProxyAPI deployment: compat auth ids are
// "openai-compatibility:<name>:<hash>" and the candidate provider key is
// "openai-compatible-<name>".
const (
	apiProvider = "openai-compatible-agnes"
	apiAccount1 = "openai-compatibility:agnes:4dab06a22e06"
	apiAccount2 = "openai-compatibility:agnes:9f676253a039"
)

func fallbackService(t *testing.T, enabled bool) *Service {
	t.Helper()
	s := quotaService(t)
	s.setAuthFallbackRuntime(enabled)
	return s
}

func bindOneAccount(t *testing.T, s *Service, keyID, authID string) {
	t.Helper()
	if err := s.Store().ReplaceKeyAuthBindings(context.Background(), keyID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: authID},
	}); err != nil {
		t.Fatal(err)
	}
}

func apiCandidates() []AuthPickCandidate {
	return []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: apiAccount1, Provider: apiProvider},
	}
}

// Key admission and credential selection are separate: a full key must not
// force a lower-priority API pick or bypass the key's concurrency policy.
func TestPickAuthForKeyKeepsPriorityWhenKeyConcurrencyIsFull(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	if err := s.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, headers := boundKey(t, s, "key-busy")
	bindOneAccount(t, s, key.ID, "account-1")
	maxConcurrent := int64(1)
	if _, err := s.Store().UpdatePluginKeyPolicy(ctx, store.PluginKeyPolicyUpdate{ID: key.ID, MaxConcurrentRequests: &maxConcurrent}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.BuildReservePlan(ctx, "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Hold the key's only slot the way an in-flight bound request would. The
	// candidates offer a usable bound account, so only the key's own limit is full.
	held, err := s.Reserve(ctx, key, plan, "in-flight")
	if err != nil {
		t.Fatal(err)
	}
	candidates := apiCandidates()
	candidates[0].Priority = 2
	candidates[1].Priority = 1
	id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5.6-sol")
	if err != nil || !handled || id != "account-1" {
		t.Fatalf("full-key priority pick = (%q, %t, %v)", id, handled, err)
	}
	if _, err := s.Reserve(ctx, key, plan, "another-in-flight"); !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("key admission must still enforce its cap: %v", err)
	}
	status, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalHits != 0 || status.LastHit != nil {
		t.Fatalf("scheduler must not record execution replay: %#v", status)
	}

	// Releasing the key slot does not change credential priority.
	if err := s.Release(ctx, held.ID, "test"); err != nil {
		t.Fatal(err)
	}
	id, handled, err = s.PickAuthForKey(ctx, headers, candidates, "gpt-5.6-sol")
	if err != nil || !handled || id != "account-1" {
		t.Fatalf("bound pick after release = (%q, %t, %v)", id, handled, err)
	}
}

// A fallback attempt does not occupy the key's own concurrency, so a key holding
// only fallback traffic still has its accounts picked first.
func TestPickAuthForKeyIgnoresFallbackReservationsForKeyConcurrency(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	if err := s.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, headers := boundKey(t, s, "key-fallback-hold")
	bindOneAccount(t, s, key.ID, "account-1")
	maxConcurrent := int64(1)
	if _, err := s.Store().UpdatePluginKeyPolicy(ctx, store.PluginKeyPolicyUpdate{ID: key.ID, MaxConcurrentRequests: &maxConcurrent}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.BuildReservePlan(ctx, "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveWithOptions(ctx, key, plan, "fallback-in-flight", ReserveOptions{Fallback: true}); err != nil {
		t.Fatal(err)
	}
	id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if err != nil || !handled || id != "account-1" {
		t.Fatalf("bound pick with only a fallback in flight = (%q, %t, %v)", id, handled, err)
	}
}

// Without a usable API provider the key's concurrency rejection stands: spilling
// must not turn a throttle into a routing change.
func TestPickAuthForKeyKeepsBoundPickWhenFallbackIsUnavailable(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	if err := s.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, headers := boundKey(t, s, "key-busy-no-api")
	bindOneAccount(t, s, key.ID, "account-1")
	maxConcurrent := int64(1)
	if _, err := s.Store().UpdatePluginKeyPolicy(ctx, store.PluginKeyPolicyUpdate{ID: key.ID, MaxConcurrentRequests: &maxConcurrent}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.BuildReservePlan(ctx, "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, key, plan, "in-flight"); err != nil {
		t.Fatal(err)
	}
	id, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{{ID: "account-1", Provider: "codex"}}, "gpt-5.6-sol")
	if err != nil || !handled || id != "account-1" {
		t.Fatalf("pick without an API provider = (%q, %t, %v)", id, handled, err)
	}
}

func TestPickAuthForKeyFallsBackToAPIProvider(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "fallback")
	bindOneAccount(t, s, key.ID, "account-9")

	id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if err != nil || !handled || id != apiAccount1 {
		t.Fatalf("fallback pick = (%q, %t, %v)", id, handled, err)
	}
}

// The old execution-replay toggle must not hide global API credentials from
// the scheduler's authorized candidate pool.
func TestPickAuthForKeyUsesAPIWithExecutionFallbackDisabled(t *testing.T) {
	s := fallbackService(t, false)
	ctx := context.Background()
	key, headers := boundKey(t, s, "no-fallback")
	bindOneAccount(t, s, key.ID, "account-9")

	id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if err != nil || !handled || id != apiAccount1 {
		t.Fatalf("API pick with execution replay disabled = (%q, %t, %v)", id, handled, err)
	}
}

// The fallback is not a general escape hatch: it must never hand a bound key an
// account of another kind, even when such an account is free.
func TestPickAuthForKeyFallbackIgnoresNonAPICandidates(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "codex-only")
	bindOneAccount(t, s, key.ID, "account-9")

	_, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: "account-2", Provider: "codex"},
	}, "gpt-5.6-sol")
	if !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("non-api fallback pick = handled:%t err:%v", handled, err)
	}
}

// A disabled API provider is an explicit operator decision and must not be used.
// An "error" one is different: it is usually stale, and only a served request can
// clear it, so the fallback still uses it (the host drops cooling credentials
// before offering candidates, which is the real protection).
func TestPickAuthForKeyFallbackSkipsDisabledAPIProvider(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "disabled-api")
	bindOneAccount(t, s, key.ID, "account-9")

	_, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: apiAccount1, Provider: apiProvider, Status: "disabled"},
	}, "gpt-5.6-sol")
	if !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("disabled api pick = handled:%t err:%v", handled, err)
	}
}

func TestPickAuthForKeyFallbackUsesErroredAPIProvider(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "errored-api")
	bindOneAccount(t, s, key.ID, "account-9")

	id, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: apiAccount1, Provider: apiProvider, Status: "error"},
	}, "gpt-5.6-sol")
	if err != nil || !handled || id != apiAccount1 {
		t.Fatalf("errored api pick = (%q, %t, %v)", id, handled, err)
	}
	// Scheduler picks are not legacy execution replays.
	status, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalHits != 0 || status.LastHit != nil {
		t.Fatalf("unexpected execution replay hits: %#v", status)
	}
}

// The fallback obeys the same concurrency cap as any other account, so it cannot
// become a way around an operator-set limit.
func TestPickAuthForKeyFallbackHonoursAPIConcurrencyLimit(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "api-busy")
	bindOneAccount(t, s, key.ID, "account-9")

	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, apiProvider, apiAccount1, 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-api", "gpt-5.6-sol")
	if err := s.AdmitAuth(ctx, "res-api", store.AuthIdentity{AuthID: apiAccount1, Provider: apiProvider}); err != nil {
		t.Fatal(err)
	}

	_, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("capped api pick = handled:%t err:%v", handled, err)
	}

	s.FinishAuthCapture("res-api")
	id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if err != nil || !handled || id != apiAccount1 {
		t.Fatalf("api pick after release = (%q, %t, %v)", id, handled, err)
	}
}

// OAuth and API credentials at equal priority share the same rotation pool.
func TestPickAuthForKeyRotatesBoundAndGlobalAPIAtEqualPriority(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "healthy")
	bindOneAccount(t, s, key.ID, "account-1")

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
		if err != nil || !handled {
			t.Fatalf("equal-priority pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		seen[id]++
	}
	if seen["account-1"] != 2 || seen[apiAccount1] != 2 {
		t.Fatalf("equal-priority mixed rotation = %#v", seen)
	}
}

// A bound account that exists but is at its own cap is a failure path too.
func TestPickAuthForKeyFallbackCoversCappedBoundAccount(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "bound-busy")
	bindOneAccount(t, s, key.ID, "account-1")

	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "account-1", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-bound", "gpt-5.6-sol")
	if err := s.AdmitAuth(ctx, "res-bound", store.AuthIdentity{AuthID: "account-1", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}

	id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if err != nil || !handled || id != apiAccount1 {
		t.Fatalf("capped bound pick = (%q, %t, %v)", id, handled, err)
	}
}

// Several API providers rotate, so one credential is not always the one paying.
func TestPickAuthForKeyFallbackRotatesAcrossAPIProviders(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "rotate-api")
	bindOneAccount(t, s, key.ID, "account-9")

	candidates := []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: apiAccount1, Provider: apiProvider},
		{ID: apiAccount2, Provider: apiProvider},
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5.6-sol")
		if err != nil || !handled {
			t.Fatalf("rotation pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		seen[id]++
	}
	if seen[apiAccount1] != 2 || seen[apiAccount2] != 2 {
		t.Fatalf("api rotation = %#v", seen)
	}
}

// Unbound plugin keys use the global priority pool without the legacy replay toggle.
func TestPickAuthForKeySchedulesUnboundGlobalAPIByPriority(t *testing.T) {
	s := fallbackService(t, false)
	_, headers := boundKey(t, s, "unbound-global")
	candidates := apiCandidates()
	candidates[0].Priority = 1
	candidates[1].Priority = 2

	id, handled, err := s.PickAuthForKey(context.Background(), headers, candidates, "gpt-5.6-sol")
	if err != nil || !handled || id != apiAccount1 {
		t.Fatalf("unbound global API pick = (%q, %t, %v)", id, handled, err)
	}
}

// The settings round trip must reach the runtime cache, because the pick path
// never reads the database for the toggle.
func TestAuthFallbackSettingsRoundTrip(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()

	settings, err := s.AuthFallbackSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled {
		t.Fatal("fallback must default to off")
	}

	updated, err := s.UpdateAuthFallbackSettings(ctx, store.AuthFallbackSettings{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || !s.authFallbackEnabled() {
		t.Fatalf("runtime cache did not observe the update: %#v", updated)
	}

	stored, err := s.Store().GetAuthFallbackSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled {
		t.Fatalf("stored settings = %#v", stored)
	}
}

func TestAuthFallbackSettingsUnavailableWithoutStore(t *testing.T) {
	var s *Service
	if _, err := s.UpdateAuthFallbackSettings(context.Background(), store.AuthFallbackSettings{Enabled: true}); err == nil {
		t.Fatal("nil service must reject an update")
	}
	if (*Service)(nil).authFallbackEnabled() {
		t.Fatal("nil service must report the fallback as disabled")
	}
}

// A plugin key that fails verification must not reach the fallback either: the
// account isolation decision happens before any account is considered.
func TestPickAuthForKeyFallbackRequiresValidKey(t *testing.T) {
	s := fallbackService(t, true)
	headers := http.Header{}
	// Well-formed key material that was never minted.
	headers.Set("Authorization", "Bearer tk-AAAAAAAAAAAAAAAA-AAAAAAAAAAAAAAAA")

	if _, handled, err := s.PickAuthForKey(context.Background(), headers, apiCandidates(), "gpt-5.6-sol"); !handled || err == nil {
		t.Fatalf("unknown key pick = handled:%t err:%v", handled, err)
	}
}

// Only legacy execution replays add fallback audit hits; an ordinary API pick
// must not be presented as an execution failure/replay.
func TestRecordUpstreamFallbackPreservesCountersAndDetails(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "recorded")
	bindOneAccount(t, s, key.ID, "account-9")

	before, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.TotalHits != 0 || before.LastHit != nil {
		t.Fatalf("fresh status = %#v", before)
	}

	if _, _, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}

	status, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalHits != 0 || status.RecentHits != 0 || status.LastHit != nil {
		t.Fatalf("scheduler pick recorded an execution replay: %#v", status)
	}
	s.RecordUpstreamFallback(ctx, key, apiProvider, "gpt-5.6-sol")
	status, err = s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Settings.Enabled || status.TotalHits != 1 || status.RecentHits != 1 {
		t.Fatalf("status after one execution replay = %#v", status)
	}
	hit := status.LastHit
	if hit == nil {
		t.Fatal("last hit was not recorded")
	}
	if hit.PluginKeyID != key.ID || hit.Label != "recorded" {
		t.Fatalf("hit key = %#v", hit)
	}
	if hit.Provider != apiProvider || hit.AuthID != apiProvider || hit.Model != "gpt-5.6-sol" {
		t.Fatalf("hit details = %#v", hit)
	}
	if hit.Reason != authFallbackReasonUpstreamError {
		t.Fatalf("hit reason = %q", hit.Reason)
	}
	if hit.At.IsZero() {
		t.Fatal("hit timestamp is zero")
	}
}

// Account-capacity routing is an ordinary scheduler decision, not a legacy
// execution replay, even when it selects an API credential.
func TestPickAuthForKeyDoesNotRecordBusyRoutingAsReplay(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "busy-reason")
	bindOneAccount(t, s, key.ID, "account-1")

	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "account-1", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-busy-reason", "gpt-5.6-sol")
	if err := s.AdmitAuth(ctx, "res-busy-reason", store.AuthIdentity{AuthID: "account-1", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}

	if _, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol"); err != nil || !handled {
		t.Fatalf("busy fallback pick = (%t, %v)", handled, err)
	}

	status, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalHits != 0 || status.LastHit != nil {
		t.Fatalf("scheduler capacity routing recorded a replay: %#v", status)
	}
}

// Neither successful scheduler picks nor failing paths inflate execution-replay
// counters, regardless of the old execution toggle.
func TestPickAuthForKeyDoesNotRecordExecutionReplay(t *testing.T) {
	ctx := context.Background()
	s := fallbackService(t, false)
	key, headers := boundKey(t, s, "no-record")
	bindOneAccount(t, s, key.ID, "account-9")

	if id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol"); err != nil || !handled || id != apiAccount1 {
		t.Fatalf("pick with execution replay disabled = (%q, %t, %v)", id, handled, err)
	}
	// Enabled, but the request offers no API provider to fall back to.
	s.setAuthFallbackRuntime(true)
	if _, _, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{{ID: "account-1", Provider: "codex"}}, "gpt-5.6-sol"); !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("no-candidate pick error = %v", err)
	}

	status, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalHits != 0 || status.LastHit != nil {
		t.Fatalf("unexpected hits recorded: %#v", status)
	}
}

// The fail-closed error is the only diagnostic channel the plugin has: the host
// logs it. It must keep the sentinel while carrying what the plugin actually saw,
// otherwise a declined fallback is indistinguishable from a disabled one.
func TestNoBoundAuthErrorCarriesDiagnostics(t *testing.T) {
	s := fallbackService(t, true)
	err := s.noBoundAuthError([]AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: apiAccount1, Provider: apiProvider, Status: "disabled"},
	})
	if !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("diagnostic error does not wrap the sentinel: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"policy=priority", "candidates=2", "api=1", "enabled_api=0",
		"codex:1", "openai-compatible-agnes:1",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic %q is missing %q", msg, want)
		}
	}
	s.setAuthFallbackRuntime(false)
	if msg := s.noBoundAuthError(nil).Error(); !strings.Contains(msg, "policy=priority") || strings.Contains(msg, "fallback=") {
		t.Fatalf("diagnostic must not imply the execution toggle controls scheduling: %q", msg)
	}
}

// The audit event must be visible in the existing audit stream too.
func TestFallbackHitAppearsInAuditStream(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, _ := boundKey(t, s, "audit-stream")
	s.RecordUpstreamFallback(ctx, key, apiProvider, "gpt-5.6-sol")
	events, err := s.Store().ListAuditEventsFiltered(ctx, store.AuditFilter{PluginKeyID: key.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].EventType != authFallbackEventType {
		t.Fatalf("audit events = %#v", events)
	}
}
