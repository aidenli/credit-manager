package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

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

// A bound key whose accounts are not in the candidate window is served by the
// API provider instead of failing, because the operator opted in.
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

// Without the opt-in the previous behaviour is untouched: the request fails
// closed rather than spending money on a shared account.
func TestPickAuthForKeyFallbackDisabledStillFailsClosed(t *testing.T) {
	s := fallbackService(t, false)
	ctx := context.Background()
	key, headers := boundKey(t, s, "no-fallback")
	bindOneAccount(t, s, key.ID, "account-9")

	id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
	if !handled || id != "" || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("disabled fallback pick = (%q, %t, %v)", id, handled, err)
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
	// It is still recorded, so the operator sees the fallback spending money.
	status, err := s.AuthFallbackStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalHits != 1 {
		t.Fatalf("hits = %d", status.TotalHits)
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

// A healthy bound account always wins; the fallback only covers the failure path.
func TestPickAuthForKeyPrefersBoundAccountOverFallback(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "healthy")
	bindOneAccount(t, s, key.ID, "account-1")

	for i := 0; i < 3; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol")
		if err != nil || !handled || id != "account-1" {
			t.Fatalf("healthy pick %d = (%q, %t, %v)", i, id, handled, err)
		}
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

// Keys without bindings stay entirely with the host scheduler.
func TestPickAuthForKeyFallbackLeavesUnboundKeysAlone(t *testing.T) {
	s := fallbackService(t, true)
	_, headers := boundKey(t, s, "unbound-fallback")

	id, handled, err := s.PickAuthForKey(context.Background(), headers, apiCandidates(), "gpt-5.6-sol")
	if err != nil || handled || id != "" {
		t.Fatalf("unbound pick = (%q, %t, %v)", id, handled, err)
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

// Every fallback decision is persisted, because from the client's point of view
// the request simply succeeded.
func TestPickAuthForKeyRecordsFallbackHit(t *testing.T) {
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
	if !status.Settings.Enabled || status.TotalHits != 1 || status.RecentHits != 1 {
		t.Fatalf("status after one hit = %#v", status)
	}
	hit := status.LastHit
	if hit == nil {
		t.Fatal("last hit was not recorded")
	}
	if hit.PluginKeyID != key.ID || hit.Label != "recorded" {
		t.Fatalf("hit key = %#v", hit)
	}
	if hit.Provider != apiProvider || hit.AuthID != apiAccount1 || hit.Model != "gpt-5.6-sol" {
		t.Fatalf("hit details = %#v", hit)
	}
	if hit.Reason != authFallbackReasonNotOffered {
		t.Fatalf("hit reason = %q", hit.Reason)
	}
	if hit.At.IsZero() {
		t.Fatal("hit timestamp is zero")
	}
}

// A bound account that is offered but busy is recorded with its own reason, so
// the console can tell "never routed here" from "at capacity".
func TestPickAuthForKeyRecordsBusyReason(t *testing.T) {
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
	if status.TotalHits != 1 || status.LastHit == nil || status.LastHit.Reason != authFallbackReasonBusy {
		t.Fatalf("status = %#v (last=%#v)", status, status.LastHit)
	}
}

// Disabled and failing paths must not inflate the counter.
func TestPickAuthForKeyDoesNotRecordWithoutFallback(t *testing.T) {
	ctx := context.Background()
	s := fallbackService(t, false)
	key, headers := boundKey(t, s, "no-record")
	bindOneAccount(t, s, key.ID, "account-9")

	if _, _, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol"); !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("disabled pick error = %v", err)
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
		"fallback=enabled", "candidates=2", "api=1", "usable=0",
		"codex:1", "openai-compatible-agnes:1",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic %q is missing %q", msg, want)
		}
	}
	s.setAuthFallbackRuntime(false)
	if msg := s.noBoundAuthError(nil).Error(); !strings.Contains(msg, "fallback=disabled") {
		t.Fatalf("disabled diagnostic = %q", msg)
	}
}

// The audit event must be visible in the existing audit stream too.
func TestFallbackHitAppearsInAuditStream(t *testing.T) {
	s := fallbackService(t, true)
	ctx := context.Background()
	key, headers := boundKey(t, s, "audit-stream")
	bindOneAccount(t, s, key.ID, "account-9")

	if _, _, err := s.PickAuthForKey(ctx, headers, apiCandidates(), "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	events, err := s.Store().ListAuditEventsFiltered(ctx, store.AuditFilter{PluginKeyID: key.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].EventType != authFallbackEventType {
		t.Fatalf("audit events = %#v", events)
	}
}
