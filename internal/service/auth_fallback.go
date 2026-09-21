package service

import (
	"context"
	"errors"
	"strings"

	"github.com/yuluo688/credit-manager/internal/store"
)

// API-provider fallback for bound keys.
//
// A key that binds accounts fails closed when none of them can serve the
// request. That is the right default, because a binding is an access boundary
// rather than a scheduling hint. It does mean, however, that a codex quota
// outage or a cooling credential turns into a hard failure for every key bound
// to it. When the operator opts in, such a request is served by an API provider
// (an "openai-compatible-*" provider such as a paid relay) instead.
//
// Scope: bound keys only. Unbound keys keep returning handled=false and stay
// entirely with the host's own selector, where API providers already compete for
// candidates on their own.
//
// The fallback may only ever return an account taken from the current scheduler
// request. The host validates the returned auth id against that exact list
// (CLIProxyAPI internal/pluginhost/scheduler.go normalizeSchedulerResponse and
// sdk/cliproxy/auth/conductor_selection.go pickSchedulerAuthByID); an id outside
// it is not reported as an error there. It silently degrades to handled=false and
// lets the host's selector choose among the request's candidates, which for a
// bound key means an arbitrary account the key was never granted. Never return a
// remembered or configured id here.

// authPickScopeFallback is the cursor scope for fallback rotation. It is a
// dedicated scope so fallback picks never disturb the provider-wide cursor the
// host-independent rotation uses.
const authPickScopeFallback = "\x00fallback"

// authFallbackState is the effective runtime toggle. It is cached in an atomic
// pointer so the per-request path never queries the database, and it is
// refreshed whenever the console writes new settings.
type authFallbackState struct {
	enabled bool
}

// authFallbackEnabled reports whether the operator opted into the fallback.
func (s *Service) authFallbackEnabled() bool {
	return s.authFallbackSettings().Enabled
}

// authFallbackSettings returns the effective settings, loading them from the
// database on first use.
func (s *Service) authFallbackSettings() store.AuthFallbackSettings {
	if s == nil {
		return store.DefaultAuthFallbackSettings()
	}
	if cached := s.authFallbackState.Load(); cached != nil {
		return store.AuthFallbackSettings{Enabled: cached.enabled}
	}
	s.authFallbackStateMu.Lock()
	defer s.authFallbackStateMu.Unlock()
	if cached := s.authFallbackState.Load(); cached != nil {
		return store.AuthFallbackSettings{Enabled: cached.enabled}
	}
	settings := store.DefaultAuthFallbackSettings()
	if s.store != nil {
		if stored, err := s.store.GetAuthFallbackSettings(context.Background()); err == nil {
			settings = stored
		}
	}
	s.storeAuthFallbackState(settings)
	return settings
}

func (s *Service) storeAuthFallbackState(settings store.AuthFallbackSettings) {
	s.authFallbackState.Store(&authFallbackState{enabled: settings.Enabled})
}

// AuthFallbackSettings exposes the effective settings to the management API.
func (s *Service) AuthFallbackSettings(_ context.Context) (store.AuthFallbackSettings, error) {
	return s.authFallbackSettings(), nil
}

// UpdateAuthFallbackSettings persists the toggle and refreshes the runtime cache,
// so the change applies to the next request without a host restart.
func (s *Service) UpdateAuthFallbackSettings(ctx context.Context, settings store.AuthFallbackSettings) (store.AuthFallbackSettings, error) {
	if s == nil || s.store == nil {
		return store.AuthFallbackSettings{}, errors.New("auth fallback settings unavailable")
	}
	updated, err := s.store.UpsertAuthFallbackSettings(ctx, settings)
	if err != nil {
		return store.AuthFallbackSettings{}, err
	}
	s.storeAuthFallbackState(updated)
	return updated, nil
}

// setAuthFallbackRuntime applies settings to the in-process cache only. Used by
// tests to exercise the pick path without a database round trip.
func (s *Service) setAuthFallbackRuntime(enabled bool) {
	s.storeAuthFallbackState(store.AuthFallbackSettings{Enabled: enabled})
}

// authProviderIsAPI reports whether a host provider key belongs to an
// OpenAI-compatible API provider. Those keys are "openai-compatible-<name>"
// (CLIProxyAPI internal/util/provider.go OpenAICompatibleProviderKey) and
// authLimitProvider keeps them unaliased, so the prefix is the whole test.
//
// Codex API-key credentials are deliberately not treated as fallbacks: they
// share the "codex" provider with the OAuth accounts a key binds to, and the
// host strips the api_key attribute from scheduler candidates, so they cannot be
// told apart from a bound OAuth account anyway.
func authProviderIsAPI(provider string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(provider)), "openai-compatible")
}

// pickFallbackAPIAuthLocked chooses the API provider that should serve a bound
// key whose own accounts are all unavailable. It returns false when the fallback
// is disabled, when the request offers no usable API provider, or when every
// eligible one is at its concurrency cap or warmup-held, in which case the caller
// fails closed exactly as before.
//
// Callers must hold authMu.
func (s *Service) pickFallbackAPIAuthLocked(limits map[string]int64, candidates []AuthPickCandidate) (AuthPickCandidate, bool) {
	if !s.authFallbackEnabled() {
		return AuthPickCandidate{}, false
	}
	pool := make([]AuthPickCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !authProviderIsAPI(authLimitProvider(candidate.Provider)) {
			continue
		}
		if strings.TrimSpace(candidate.ID) == "" || authStatusUnusable(candidate.Status) {
			continue
		}
		pool = append(pool, candidate)
	}
	if len(pool) == 0 {
		return AuthPickCandidate{}, false
	}
	// The fallback is a shared account set, so it obeys the same concurrency and
	// warmup rules as any other candidate instead of becoming a bypass for them.
	available, _ := s.filterAvailableAuthLocked(limits, pool)
	if len(available) == 0 {
		return AuthPickCandidate{}, false
	}
	return s.nextAuthPickLocked(authPickScopeFallback, available), true
}
