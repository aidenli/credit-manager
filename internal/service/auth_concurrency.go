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
	Priority int
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
	chosen := s.nextAuthPickLocked("global", highestPriorityAuths(available))
	s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: strings.TrimSpace(chosen.ID), Provider: chosen.Provider})
	return strings.TrimSpace(chosen.ID), true, nil
}

// PickAuthForKey routes one scheduler decision for the key behind the request.
// Plugin keys without bindings use the global pool through this same selector,
// so host affinity cannot retain a lower-priority account. Bound keys may use
// their own accounts plus any global OpenAI-compatible API credential. Permissions and
// availability are filtered before priority; an empty authorized pool fails closed.
// Legacy execution-replay settings do not affect scheduler permissions.
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
		if _, ok := bound[provider+"\x00"+id]; len(bindings) > 0 && !ok && !authProviderIsAPI(provider) {
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
	// Key admission stays in authentication/reservation; routing must not bypass
	// it or change priority just because an API credential is shared.
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	available, _ := s.filterAvailableAuthLocked(limits, scoped)
	if len(available) == 0 {
		if len(bindings) == 0 {
			return "", true, store.ErrConcurrentLimit
		}
		return "", true, s.noBoundAuthError(candidates)
	}
	available = highestPriorityAuths(available)
	// A stable mixed-provider namespace keeps affinity independent of candidate
	// ordering. Lookup only sees the authorized, available highest-priority tier.
	const provider = "mixed"
	sessionID := ""
	if s.sessionAffinityEnabled() {
		sessionID = sessionAffinityID(headers)
	}
	// Health is advisory within the highest tier, never a reason to promote a
	// lower-priority credential. An errored-only tier can still recover by serving.
	pickFrom := available
	if healthy := withoutErroredAuths(available); len(healthy) > 0 {
		pickFrom = healthy
	}
	chosen := s.chooseBoundAuthLocked(key.ID+"\x00"+provider, provider, sessionID, strings.TrimSpace(model), pickFrom, time.Now())
	s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: strings.TrimSpace(chosen.ID), Provider: chosen.Provider})
	return strings.TrimSpace(chosen.ID), true, nil
}

// highestPriorityAuths runs after permission and availability filtering. Priority
// may be negative; zero is not a lower bound.
func highestPriorityAuths(candidates []AuthPickCandidate) []AuthPickCandidate {
	if len(candidates) == 0 {
		return nil
	}
	priority := candidates[0].Priority
	for _, candidate := range candidates[1:] {
		if candidate.Priority > priority {
			priority = candidate.Priority
		}
	}
	out := make([]AuthPickCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Priority == priority {
			out = append(out, candidate)
		}
	}
	return out
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
		if provider == "" || id == "" || authStatusDisabled(candidate.Status) {
			continue
		}
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
