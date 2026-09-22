package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/keys"
	"github.com/yuluo688/credit-manager/internal/store"
)

// AuthPickCandidate is one host auth offered to the plugin scheduler.
type AuthPickCandidate struct {
	ID       string
	Provider string
	// Status is the host-visible lifecycle state (unknown, active, pending,
	// refreshing, error, disabled).
	Status string
}

// authStatusDisabled reports whether the host explicitly disabled this credential.
// It is the only status that vetoes a selection: an explicit disable is operator
// intent, while everything else the host still offers is usable.
func authStatusDisabled(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "disabled")
}

// authStatusErrored reports the host's advisory error flag. It never vetoes a pick,
// because an account can only clear that flag by serving a request — honoring it
// would lock the credential out permanently and hand every request of a bound key
// to the API-provider fallback. A healthy sibling is still preferred.
func authStatusErrored(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "error")
}

// ErrNoBoundAuthAvailable fails closed when a key binds OAuth accounts but none
// of them can serve the request right now. It must never fall back to accounts
// the key was not granted.
var ErrNoBoundAuthAvailable = errors.New("bound oauth accounts are currently unavailable")

func (s *Service) SetAuthConcurrencyLimit(ctx context.Context, provider, authID string, maxConcurrent int64) (AuthQuotaOverviewItem, error) {
	provider, authID = authLimitProvider(provider), strings.TrimSpace(authID)
	if provider == "" || authID == "" {
		return AuthQuotaOverviewItem{}, fmt.Errorf("%w: provider and auth id are required", store.ErrInvalidArgument)
	}
	if maxConcurrent < 0 {
		return AuthQuotaOverviewItem{}, fmt.Errorf("%w: max concurrent requests must not be negative", store.ErrInvalidArgument)
	}
	if err := s.store.UpsertAuthConcurrencyLimit(ctx, provider, authID, maxConcurrent); err != nil {
		return AuthQuotaOverviewItem{}, err
	}
	source, files, err := s.authQuotaFiles(ctx)
	if err != nil {
		return s.withAuthConcurrency(ctx, AuthQuotaOverviewItem{AuthID: authID, Provider: provider, MaxConcurrentRequests: maxConcurrent}), nil
	}
	for _, file := range files {
		if !matchAuthQuotaFile(file, provider, authID, "") {
			continue
		}
		item, ok := s.loadAuthQuotaItem(ctx, source, "", file, false)
		if !ok {
			continue
		}
		return s.withAuthConcurrency(ctx, item), nil
	}
	item := AuthQuotaOverviewItem{AuthID: authID, Provider: provider, MaxConcurrentRequests: maxConcurrent}
	return s.withAuthConcurrency(ctx, item), nil
}

// AuthConcurrencyTarget identifies one auth row for a batch concurrency update.
type AuthConcurrencyTarget struct {
	Provider string `json:"provider"`
	AuthID   string `json:"auth_id"`
}

// AuthConcurrencyBatchResult reports how many auth rows a batch update changed.
type AuthConcurrencyBatchResult struct {
	Updated               int   `json:"updated"`
	MaxConcurrentRequests int64 `json:"max_concurrent_requests"`
}

func (s *Service) SetAuthConcurrencyLimits(ctx context.Context, filter AuthQuotaFilter, targets []AuthConcurrencyTarget, maxConcurrent int64) (AuthConcurrencyBatchResult, error) {
	if maxConcurrent < 0 {
		return AuthConcurrencyBatchResult{}, fmt.Errorf("%w: max concurrent requests must not be negative", store.ErrInvalidArgument)
	}
	limits, err := s.authConcurrencyBatchLimits(ctx, filter, targets, maxConcurrent)
	if err != nil {
		return AuthConcurrencyBatchResult{}, err
	}
	if err := s.store.UpsertAuthConcurrencyLimits(ctx, limits); err != nil {
		return AuthConcurrencyBatchResult{}, err
	}
	return AuthConcurrencyBatchResult{Updated: len(limits), MaxConcurrentRequests: maxConcurrent}, nil
}

func (s *Service) authConcurrencyBatchLimits(ctx context.Context, filter AuthQuotaFilter, targets []AuthConcurrencyTarget, maxConcurrent int64) ([]store.AuthConcurrencyLimit, error) {
	_, files, err := s.authQuotaFiles(ctx)
	if err != nil {
		return nil, err
	}
	filter = normalizeAuthQuotaFilter(filter)
	want := map[string]struct{}{}
	for _, target := range targets {
		provider, authID := authLimitProvider(target.Provider), strings.TrimSpace(target.AuthID)
		if provider == "" || authID == "" {
			continue
		}
		want[provider+"\x00"+authID] = struct{}{}
	}
	restrict := len(want) > 0
	out := make([]store.AuthConcurrencyLimit, 0, len(files))
	seen := map[string]struct{}{}
	for _, file := range files {
		provider := quotaProvider(file.Provider)
		authID := first(file.ID, file.Name, file.AuthIndex)
		if provider == "" || authID == "" {
			continue
		}
		if !authQuotaFileMatchesProvider(file, filter.Provider) || !authQuotaFileMatchesQuery(file, provider, filter.Q) {
			continue
		}
		if saved, savedErr := s.store.GetAuthQuotaSnapshot(ctx, provider, authID); savedErr == nil && isAuthQuotaAPIKeySentinel(saved) {
			continue
		}
		key := provider + "\x00" + authID
		if restrict {
			if _, ok := want[key]; !ok {
				continue
			}
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, store.AuthConcurrencyLimit{Provider: provider, AuthID: authID, MaxConcurrentRequests: maxConcurrent})
	}
	return out, nil
}

func (s *Service) AdmitAuth(ctx context.Context, reservationID string, auth store.AuthIdentity) error {
	if s == nil || strings.TrimSpace(reservationID) == "" || auth.Empty() {
		return nil
	}
	provider, authID := authLimitIdentity(auth)
	if provider == "" || authID == "" {
		s.bindAuthCapture(reservationID, auth)
		return nil
	}
	limit, err := s.store.GetAuthConcurrencyLimit(ctx, provider, authID)
	if err != nil {
		return err
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	if limit > 0 && s.activeAuthRequestsLocked(provider, authID, reservationID) >= limit {
		return store.ErrConcurrentLimit
	}
	s.bindAuthCaptureLocked(reservationID, auth)
	return nil
}

func (s *Service) PickAuth(ctx context.Context, candidates []AuthPickCandidate) (authID string, handled bool, err error) {
	if s == nil || len(candidates) == 0 {
		return "", false, nil
	}
	limits, err := s.store.ListAuthConcurrencyLimits(ctx)
	if err != nil {
		return "", false, err
	}
	anyLimit := false
	for _, candidate := range candidates {
		if authConcurrencyLimitOf(limits, candidate) > 0 {
			anyLimit = true
			break
		}
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	available, hasWarmupHold := s.filterAvailableAuthLocked(limits, candidates)
	if !anyLimit && !hasWarmupHold {
		return "", false, nil
	}
	if len(available) == 0 {
		return "", true, store.ErrConcurrentLimit
	}
	chosen := s.nextAuthPickLocked("", available)
	s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: strings.TrimSpace(chosen.ID), Provider: chosen.Provider})
	return strings.TrimSpace(chosen.ID), true, nil
}

// PickAuthForKey routes one scheduler decision for the key behind the request.
// Keys without bindings keep the unbound behaviour unchanged. Bound keys may
// only use their own accounts; when none of them can serve the request they fall
// back to an API provider if the operator enabled that, and otherwise fail
// closed.
//
// model only participates in the session-affinity binding key (mirroring the
// host's provider::session::model cache key); selection itself is model-agnostic
// because the host has already filtered candidates by model.
func (s *Service) PickAuthForKey(ctx context.Context, headers http.Header, candidates []AuthPickCandidate, model string) (authID string, handled bool, err error) {
	if s == nil {
		return "", false, nil
	}
	rawKey := bearerToken(headers)
	if _, parseErr := keys.Parse(rawKey); parseErr != nil {
		// Requests not carrying a plugin key remain under the host scheduler.
		return s.PickAuth(ctx, candidates)
	}
	key, keyErr := s.LookupPluginKey(ctx, rawKey)
	if keyErr != nil {
		// A plugin-shaped credential must not bypass account isolation when key
		// lookup or verification fails.
		return "", true, keyErr
	}
	bindings, err := s.store.ListKeyAuthBindings(ctx, key.ID)
	if err != nil {
		return "", false, err
	}
	if len(bindings) == 0 {
		return s.PickAuth(ctx, candidates)
	}
	bound := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		provider, id := authLimitProvider(binding.Provider), strings.TrimSpace(binding.AuthID)
		if provider == "" || id == "" {
			continue
		}
		bound[provider+"\x00"+id] = struct{}{}
	}
	scoped := make([]AuthPickCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		provider, id := authLimitProvider(candidate.Provider), strings.TrimSpace(candidate.ID)
		if provider == "" || id == "" {
			continue
		}
		if _, ok := bound[provider+"\x00"+id]; !ok {
			continue
		}
		// The host's status is a snapshot taken when the candidate list was built.
		// Only an explicit disable removes an account here; an advisory "error"
		// flag must not black out a key's own account, so it is handled by
		// preferring healthy siblings below instead.
		if authStatusDisabled(candidate.Status) {
			continue
		}
		scoped = append(scoped, candidate)
	}
	limits, err := s.store.ListAuthConcurrencyLimits(ctx)
	if err != nil {
		return "", false, err
	}
	// A key that already has its own concurrency limit in flight would have this
	// attempt rejected when the executor reserves, which is a hard failure for the
	// client even though the API provider could serve it. Spill to the fallback
	// instead, so the limit reads as "at most N requests carried by the key's own
	// accounts".
	keyBusy := false
	if key.MaxConcurrentRequests > 0 {
		active, err := s.store.CountActiveKeyReservations(ctx, key.ID)
		if err != nil {
			return "", false, err
		}
		keyBusy = active >= key.MaxConcurrentRequests
	}
	// The lock is released explicitly rather than deferred: the fallback audit
	// write is I/O and must not run while holding the lock shared by every pick.
	s.authMu.Lock()
	s.ensureAuthPendingLocked()
	// One branch covers both fail-closed cases: no bound account is offered for
	// this request (scoped is empty) and every bound account is at its
	// concurrency cap or warmup-held (available is empty).
	available, _ := s.filterAvailableAuthLocked(limits, scoped)
	if len(available) == 0 {
		fallback, ok := s.pickFallbackAPIAuthLocked(limits, candidates)
		reason := authFallbackReasonBusy
		if len(scoped) == 0 {
			reason = authFallbackReasonNotOffered
		}
		if ok {
			fallbackID := strings.TrimSpace(fallback.ID)
			s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: fallbackID, Provider: fallback.Provider})
			s.authMu.Unlock()
			s.recordAuthFallbackHit(ctx, key, fallback, model, reason)
			return fallbackID, true, nil
		}
		s.authMu.Unlock()
		return "", true, s.noBoundAuthError(candidates)
	}
	if keyBusy {
		if fallback, ok := s.pickFallbackAPIAuthLocked(limits, candidates); ok {
			fallbackID := strings.TrimSpace(fallback.ID)
			s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: fallbackID, Provider: fallback.Provider})
			s.authMu.Unlock()
			s.recordAuthFallbackHit(ctx, key, fallback, model, authFallbackReasonKeyBusy)
			return fallbackID, true, nil
		}
		// Without a usable API provider the ordinary bound pick stands, so the
		// client gets the key's concurrency rejection rather than a routing error.
	}
	// Bound keys keep their own cursor so one key cannot skew another key's rotation.
	// With session affinity enabled the session, not the key, decides the account.
	provider := authLimitProvider(available[0].Provider)
	sessionID := ""
	if s.sessionAffinityEnabled() {
		sessionID = sessionAffinityID(headers)
	}
	// A healthy sibling wins over an account the host flagged, but a flagged
	// account stays selectable when it is all the key has: otherwise the flag can
	// never clear (only serving a request clears it) and the key would sit on the
	// API-provider fallback forever.
	pickFrom := available
	if healthy := withoutErroredAuths(available); len(healthy) > 0 {
		pickFrom = healthy
	}
	chosen := s.chooseBoundAuthLocked(key.ID+"\x00"+provider, provider, sessionID, strings.TrimSpace(model), pickFrom, time.Now())
	s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: strings.TrimSpace(chosen.ID), Provider: chosen.Provider})
	s.authMu.Unlock()
	return strings.TrimSpace(chosen.ID), true, nil
}

// withoutErroredAuths returns the candidates the host did not flag as errored.
func withoutErroredAuths(candidates []AuthPickCandidate) []AuthPickCandidate {
	out := make([]AuthPickCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if authStatusErrored(candidate.Status) {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

// filterAvailableAuthLocked drops candidates that are warmup-held or already at
// their concurrency cap. Callers must hold authMu.
func (s *Service) filterAvailableAuthLocked(limits map[string]int64, candidates []AuthPickCandidate) (available []AuthPickCandidate, hasWarmupHold bool) {
	available = make([]AuthPickCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		limit := authConcurrencyLimitOf(limits, candidate)
		provider, id := authLimitProvider(candidate.Provider), strings.TrimSpace(candidate.ID)
		if s.authWarmupBusyLocked(provider, id) {
			hasWarmupHold = true
			continue
		}
		if limit > 0 && provider != "" && id != "" && s.activeAuthRequestsLocked(provider, id, "") >= limit {
			continue
		}
		available = append(available, candidate)
	}
	return available, hasWarmupHold
}

func (s *Service) withAuthConcurrency(ctx context.Context, item AuthQuotaOverviewItem) AuthQuotaOverviewItem {
	items := []AuthQuotaOverviewItem{item}
	s.attachAuthConcurrency(ctx, items)
	return items[0]
}

func (s *Service) attachAuthConcurrency(ctx context.Context, items []AuthQuotaOverviewItem) {
	if s == nil || len(items) == 0 {
		return
	}
	limits, err := s.store.ListAuthConcurrencyLimits(ctx)
	if err != nil {
		limits = map[string]int64{}
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	for i := range items {
		provider, authID := authLimitProvider(items[i].Provider), strings.TrimSpace(items[i].AuthID)
		if provider != "" && authID != "" {
			items[i].MaxConcurrentRequests = limits[provider+"\x00"+authID]
		}
		items[i].ActiveRequests = s.activeAuthRequestsLocked(provider, authID, "")
	}
}

func (s *Service) bindAuthCapture(reservationID string, auth store.AuthIdentity) {
	if s == nil {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.bindAuthCaptureLocked(reservationID, auth)
}

func (s *Service) bindAuthCaptureLocked(reservationID string, auth store.AuthIdentity) {
	s.ensureAuthPendingLocked()
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" || auth.Empty() {
		return
	}
	pending := s.authPending[reservationID]
	if pending == nil {
		return
	}
	pending.auth = auth
	pending.hasAuth = true
}

func (s *Service) bindOldestUnattributedLocked(auth store.AuthIdentity) {
	if auth.Empty() {
		return
	}
	var oldest *pendingAuthCapture
	for _, pending := range s.authPending {
		if pending == nil || !pending.active || pending.hasAuth {
			continue
		}
		if oldest == nil || pending.startedAt.Before(oldest.startedAt) {
			oldest = pending
		}
	}
	if oldest == nil {
		return
	}
	oldest.auth = auth
	oldest.hasAuth = true
}

// authPickScopeAffinity is a cursor-scope sentinel used by the session-affinity
// path. It deliberately resolves to one shared cursor instead of a per-key one:
// sessions are bound on their first pick, so the first pick of each session is
// what spreads sessions across accounts.
const authPickScopeAffinity = "\x00affinity"

// nextAuthPickLocked round-robins inside one cursor scope.
//
// An empty scope keeps the previous semantics: the provider-wide cursor, which
// callers that already scoped by key pass explicitly.
func (s *Service) nextAuthPickLocked(scope string, available []AuthPickCandidate) AuthPickCandidate {
	if s.authPickCursor == nil {
		s.authPickCursor = map[string]int{}
	}
	key := scope
	switch key {
	case authPickScopeAffinity:
		key = "auth"
	case "":
		key = authLimitProvider(available[0].Provider)
	}
	if key == "" {
		key = "auth"
	}
	i := s.authPickCursor[key] % len(available)
	s.authPickCursor[key] = i + 1
	return available[i]
}

func (s *Service) activeAuthRequestsLocked(provider, authID, exceptReservation string) int64 {
	if authID == "" {
		return 0
	}
	provider = authLimitProvider(provider)
	exceptReservation = strings.TrimSpace(exceptReservation)
	var n int64
	for id, pending := range s.authPending {
		if pending == nil || !pending.active || !pending.hasAuth || id == exceptReservation {
			continue
		}
		pendingProvider, pendingID := authLimitIdentity(pending.auth)
		if pendingID == authID && (provider == "" || pendingProvider == provider) {
			n++
		}
	}
	for _, auth := range s.warmupHolds {
		holdProvider, holdID := authLimitIdentity(auth)
		if holdID == authID && (provider == "" || holdProvider == provider) {
			n++
		}
	}
	return n
}

func (s *Service) authWarmupBusyLocked(provider, authID string) bool {
	provider, authID = authLimitProvider(provider), strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	for _, auth := range s.warmupHolds {
		holdProvider, holdID := authLimitIdentity(auth)
		if holdID == authID && (provider == "" || holdProvider == provider) {
			return true
		}
	}
	return false
}

func authConcurrencyLimitOf(limits map[string]int64, candidate AuthPickCandidate) int64 {
	provider, authID := authLimitProvider(candidate.Provider), strings.TrimSpace(candidate.ID)
	if provider == "" || authID == "" || len(limits) == 0 {
		return 0
	}
	return limits[provider+"\x00"+authID]
}

func authLimitIdentity(auth store.AuthIdentity) (provider, authID string) {
	return authLimitProvider(auth.Provider), first(auth.AuthID, auth.AuthIndex)
}

func authLimitProvider(provider string) string {
	trimmed := strings.ToLower(strings.TrimSpace(provider))
	// An API-key provider key is "openai-compatible-<name>". It contains the
	// substring "openai", so the vendor aliasing below would fold it into "codex".
	// Keep it as-is: bindings match on exactly this string.
	// ponytail: fixed here rather than in quotaProvider, which also buckets quota
	// snapshots and would migrate existing compat snapshots between buckets.
	if strings.HasPrefix(trimmed, "openai-compatible") {
		return trimmed
	}
	if normalized := quotaProvider(provider); normalized != "" {
		return normalized
	}
	return trimmed
}
