package service

import (
	"context"
	"errors"
	"net/http"
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

// An API provider the host already declared unusable must not be selected.
func TestPickAuthForKeyFallbackSkipsUnusableAPIProvider(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"error", "disabled"} {
		t.Run(status, func(t *testing.T) {
			s := fallbackService(t, true)
			key, headers := boundKey(t, s, "bad-api")
			bindOneAccount(t, s, key.ID, "account-9")

			_, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
				{ID: "account-1", Provider: "codex"},
				{ID: apiAccount1, Provider: apiProvider, Status: status},
			}, "gpt-5.6-sol")
			if !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
				t.Fatalf("unusable api pick = handled:%t err:%v", handled, err)
			}
		})
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
