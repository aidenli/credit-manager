package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// Session affinity for bound keys.
//
// This mirrors the host's routing.session-affinity: a session keeps using the
// same OAuth account for a while instead of rotating on every request. It only
// ever chooses inside the key's own binding set, so it stays an access-control
// boundary rather than a selection hint.
//
// The host's selector cannot provide this for bound keys: when the plugin
// handles a pick, the host's SessionAffinitySelector is never consulted. See
// CLIProxyAPI sdk/cliproxy/auth/conductor_selection.go (handled==true returns
// before the selector runs) and selector.go (cacheKey = provider::session::model).
//
// State is per-process and intentionally not persisted: a restart simply starts
// new sessions, which is the same behaviour as the rotation cursor.
const (
	// sessionAffinityMaxEntries bounds the map so a long-running process cannot
	// grow it without limit when sessions never return.
	sessionAffinityMaxEntries = 10000
)

type sessionBinding struct {
	authID string
	// provider keeps a binding from crossing providers, mirroring the host's
	// "provider::session::model" cache key.
	provider string
	expires  time.Time
}

// sessionAffinityState is the effective runtime toggle. It is cached in an
// atomic pointer so the per-request hot path never queries the database, and it
// is refreshed whenever the console writes new settings.
type sessionAffinityState struct {
	enabled bool
	ttl     time.Duration
}

// sessionAffinitySettings returns the effective settings.
//
// Precedence: the database row (written by the console) wins; when no row exists
// the config file supplies the default. Config therefore acts as the deployment
// default and the database as the runtime override.
func (s *Service) sessionAffinitySettings() store.SessionAffinitySettings {
	if s == nil {
		return store.DefaultSessionAffinitySettings()
	}
	if cached := s.sessionAffinityState.Load(); cached != nil {
		return store.SessionAffinitySettings{Enabled: cached.enabled, TTL: cached.ttl}
	}
	s.sessionAffinityStateMu.Lock()
	defer s.sessionAffinityStateMu.Unlock()
	if cached := s.sessionAffinityState.Load(); cached != nil {
		return store.SessionAffinitySettings{Enabled: cached.enabled, TTL: cached.ttl}
	}
	settings := store.DefaultSessionAffinitySettings()
	if configured := s.cfg.SessionAffinity; configured.Enabled || configured.TTL > 0 {
		settings.Enabled = configured.Enabled
		if configured.TTL > 0 {
			settings.TTL = configured.TTL
		}
	}
	if s.store != nil {
		if stored, err := s.store.GetAuthSessionAffinitySettings(context.Background()); err == nil {
			settings = stored
		}
	}
	s.storeSessionAffinityState(settings)
	return settings
}

func (s *Service) storeSessionAffinityState(settings store.SessionAffinitySettings) {
	ttl := settings.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	s.sessionAffinityState.Store(&sessionAffinityState{enabled: settings.Enabled, ttl: ttl})
}

// AuthSessionAffinitySettings exposes the effective settings to the management API.
func (s *Service) AuthSessionAffinitySettings(_ context.Context) (store.SessionAffinitySettings, error) {
	return s.sessionAffinitySettings(), nil
}

// UpdateAuthSessionAffinitySettings persists the toggle and refreshes the runtime
// cache, so the change applies to the next request without a host restart.
func (s *Service) UpdateAuthSessionAffinitySettings(ctx context.Context, settings store.SessionAffinitySettings) (store.SessionAffinitySettings, error) {
	if s == nil || s.store == nil {
		return store.SessionAffinitySettings{}, errors.New("session affinity settings unavailable")
	}
	updated, err := s.store.UpsertAuthSessionAffinitySettings(ctx, settings)
	if err != nil {
		return store.SessionAffinitySettings{}, err
	}
	s.storeSessionAffinityState(updated)
	return updated, nil
}

// setSessionAffinityRuntime applies settings to the in-process cache only. Used
// by tests to exercise the hot path without a database round trip.
func (s *Service) setSessionAffinityRuntime(enabled bool, ttl time.Duration) {
	s.storeSessionAffinityState(store.SessionAffinitySettings{Enabled: enabled, TTL: ttl})
}

// sessionAffinityEnabled reports whether the operator opted in.
func (s *Service) sessionAffinityEnabled() bool {
	return s.sessionAffinitySettings().Enabled
}

func (s *Service) sessionAffinityTTL() time.Duration {
	ttl := s.sessionAffinitySettings().TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	return ttl
}

// sessionAffinityID extracts a stable session identifier from request headers.
// The precedence mirrors the host's extractSessionIDs so the account a session
// sticks to is the same one the host would consider, but only header sources are
// available here (the scheduler does not receive the request body).
func sessionAffinityID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	headerValue := func(name string) string {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
		// Case-insensitive fallback for odd clients.
		for key, values := range headers {
			if !strings.EqualFold(key, name) || len(values) == 0 {
				continue
			}
			if value := strings.TrimSpace(values[0]); value != "" {
				return value
			}
		}
		return ""
	}
	for _, candidate := range []struct{ name, prefix string }{
		{"X-Claude-Code-Session-Id", "claude:"},
		{"Session-Id", "codex:"},
		{"Session_id", "codex:"},
		{"X-Session-ID", "header:"},
		{"X-Session-Affinity", "affinity:"},
		{"X-Client-Request-Id", "clientreq:"},
	} {
		if value := headerValue(candidate.name); value != "" {
			return candidate.prefix + value
		}
	}
	return ""
}

func sessionAffinityKey(provider, sessionID, model string) string {
	return provider + "::" + sessionID + "::" + model
}

// bindSessionAffinityLocked records the account for a session and refreshes the
// TTL. Callers must hold authMu.
func (s *Service) bindSessionAffinityLocked(key, provider, authID string, now time.Time) {
	if key == "" || authID == "" {
		return
	}
	if s.sessionAffinity == nil {
		s.sessionAffinity = map[string]sessionBinding{}
	}
	s.sessionAffinity[key] = sessionBinding{authID: authID, provider: provider, expires: now.Add(s.sessionAffinityTTL())}
	if len(s.sessionAffinity) > sessionAffinityMaxEntries {
		s.pruneSessionAffinityLocked(now)
	}
}

// pruneSessionAffinityLocked drops expired bindings. Callers must hold authMu.
func (s *Service) pruneSessionAffinityLocked(now time.Time) {
	for key, binding := range s.sessionAffinity {
		if !binding.expires.After(now) {
			delete(s.sessionAffinity, key)
		}
	}
}

// sessionAffinityLookupLocked returns the bound auth for a session when it is
// still one of the currently usable candidates. Access refreshes the TTL, so an
// active session stays sticky, matching the host's GetAndRefresh behaviour.
// Callers must hold authMu.
func (s *Service) sessionAffinityLookupLocked(key string, available []AuthPickCandidate, now time.Time) (AuthPickCandidate, bool) {
	if key == "" || len(available) == 0 {
		return AuthPickCandidate{}, false
	}
	binding, ok := s.sessionAffinity[key]
	if !ok {
		return AuthPickCandidate{}, false
	}
	if !binding.expires.After(now) {
		delete(s.sessionAffinity, key)
		return AuthPickCandidate{}, false
	}
	for _, candidate := range available {
		if candidate.ID != binding.authID {
			continue
		}
		// The bound account is still usable: keep the session on it.
		binding.expires = now.Add(s.sessionAffinityTTL())
		s.sessionAffinity[key] = binding
		return candidate, true
	}
	// Bound account is no longer usable (concurrency cap, warmup, or removed
	// from the candidate set). Fall through so the caller reselects and rebinds,
	// rather than pinning the session to a dead account.
	return AuthPickCandidate{}, false
}

// chooseBoundAuthLocked picks the account for one bound-key request.
//
// With session affinity enabled and a known session, an existing usable binding
// wins; otherwise it rotates and then binds the session to the chosen account.
// With affinity disabled this is exactly the previous rotation behaviour
// (keyScope is the per-key cursor scope the caller previously passed).
// Callers must hold authMu.
func (s *Service) chooseBoundAuthLocked(keyScope, provider, sessionID, model string, available []AuthPickCandidate, now time.Time) AuthPickCandidate {
	if len(available) == 0 {
		return AuthPickCandidate{}
	}
	if s.sessionAffinityEnabled() && sessionID != "" {
		key := sessionAffinityKey(provider, sessionID, model)
		if bound, ok := s.sessionAffinityLookupLocked(key, available, now); ok {
			return bound
		}
		// First pick for this session: rotate with the shared cursor, then bind.
		chosen := s.nextAuthPickLocked(authPickScopeAffinity, available)
		s.bindSessionAffinityLocked(key, authLimitProvider(chosen.Provider), chosen.ID, now)
		return chosen
	}
	return s.nextAuthPickLocked(keyScope, available)
}
