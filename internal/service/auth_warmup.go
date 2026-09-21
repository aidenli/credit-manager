package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

// AuthWarmupRequest is a credential-pinned synthetic model request. The
// executor belongs to the plugin package so the service never handles auth JSON
// or OAuth secrets.
type AuthWarmupRequest struct {
	RunID         string
	Auth          store.AuthIdentity
	Models        []string
	EntryProtocol string
	RequestedAt   time.Time
}

type AuthWarmupResult struct {
	Usage money.TokenUsage
	Model string
}

type AuthWarmupExecutor interface {
	ExecuteAuthWarmup(context.Context, AuthWarmupRequest) (AuthWarmupResult, error)
}

func (s *Service) AuthWarmupSettings(ctx context.Context) (store.AuthWarmupSettings, error) {
	if s == nil || s.store == nil {
		return store.AuthWarmupSettings{}, errors.New("auth quota warmup unavailable")
	}
	return s.store.GetAuthWarmupSettings(ctx)
}

func (s *Service) UpdateAuthWarmupSettings(ctx context.Context, settings store.AuthWarmupSettings) (store.AuthWarmupSettings, error) {
	if s == nil || s.store == nil {
		return store.AuthWarmupSettings{}, errors.New("auth quota warmup unavailable")
	}
	updated, err := s.store.UpsertAuthWarmupSettings(ctx, settings)
	if err != nil {
		return store.AuthWarmupSettings{}, err
	}
	if authWarmupSchedulesEnabled(updated.Schedules) {
		s.StartAuthWarmup()
	} else {
		s.StopAuthWarmup()
	}
	return updated, nil
}

func (s *Service) SetAuthWarmupExecutor(executor AuthWarmupExecutor) {
	if s == nil {
		return
	}
	s.warmupMu.Lock()
	s.warmupExecutor = executor
	s.warmupMu.Unlock()
}

func (s *Service) authWarmupExecutorValue() AuthWarmupExecutor {
	if s == nil {
		return nil
	}
	s.warmupMu.RLock()
	defer s.warmupMu.RUnlock()
	return s.warmupExecutor
}

// StartAuthWarmup starts the opt-in daily scheduler after the plugin has
// attached its host callback executor. A reconfigure always replaces it.
func (s *Service) StartAuthWarmup() {
	if s == nil || s.authWarmupExecutorValue() == nil {
		return
	}
	settings, err := s.store.GetAuthWarmupSettings(context.Background())
	if err != nil || !authWarmupSchedulesEnabled(settings.Schedules) {
		s.StopAuthWarmup()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.warmupMu.Lock()
	previous := s.warmupCancel
	s.warmupCancel = cancel
	s.warmupMu.Unlock()
	if previous != nil {
		previous()
	}
	go s.runAuthWarmup(ctx)
}

func authWarmupSchedulesEnabled(schedules []store.AuthWarmupSchedule) bool {
	for _, schedule := range schedules {
		if schedule.Enabled {
			return true
		}
	}
	return false
}

func (s *Service) StopAuthWarmup() {
	if s == nil {
		return
	}
	s.warmupMu.Lock()
	cancel := s.warmupCancel
	s.warmupCancel = nil
	s.warmupMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) runAuthWarmup(ctx context.Context) {
	// Check immediately so a plugin reload during its scheduled minute does not
	// miss the run, then keep the inexpensive schedule check minute-aligned.
	s.runScheduledAuthWarmups(ctx, time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.runScheduledAuthWarmups(ctx, now)
		}
	}
}

func (s *Service) runScheduledAuthWarmups(ctx context.Context, now time.Time) {
	if s == nil || ctx.Err() != nil {
		return
	}
	settings, err := s.store.GetAuthWarmupSettings(ctx)
	if err != nil || len(settings.Schedules) == 0 {
		return
	}
	_, files, err := s.authQuotaFiles(ctx)
	if err != nil {
		return
	}
	parallel := settings.MaxParallel
	if parallel < 1 {
		parallel = 1
	}
	for _, schedule := range settings.Schedules {
		if !schedule.Enabled {
			continue
		}
		for _, target := range schedule.Auths {
			file, ok := findWarmupAuthFile(files, target)
			if !ok || quotaProvider(file.Provider) == "" || !authWarmupIsDue(settings, schedule, file, now) {
				continue
			}
			file, schedule := file, schedule
			sem := s.authWarmupSemaphore(warmupAuthSlotKey(file), parallel)
			select {
			case sem <- struct{}{}:
			default:
				// A later ticker iteration can catch this task while it remains
				// inside the bounded catch-up window below.
				continue
			}
			go func() {
				defer func() { <-sem }()
				// One claim per task/auth/clock so catch-up ticks do not repeat the same due time.
				_, _ = s.warmupAuthQuota(ctx, "", file.Provider, first(file.ID, file.Name, file.AuthIndex), file.AuthIndex, scheduledWarmupKey(schedule, now), schedule)
			}()
		}
	}
}

func warmupAuthSlotKey(file AuthQuotaFile) string {
	return quotaProvider(file.Provider) + "\x00" + first(file.ID, file.Name, file.AuthIndex)
}

func (s *Service) authWarmupSemaphore(slot string, limit int) chan struct{} {
	if limit < 1 {
		limit = 1
	}
	slot = strings.TrimSpace(slot)
	if slot == "" {
		slot = "_"
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	if s.warmupSems == nil {
		s.warmupSems = make(map[string]chan struct{})
		s.warmupSemLimit = limit
	} else if s.warmupSemLimit != limit && !authWarmupSemaphoresBusy(s.warmupSems) {
		s.warmupSems = make(map[string]chan struct{})
		s.warmupSemLimit = limit
	}
	sem := s.warmupSems[slot]
	if sem == nil {
		sem = make(chan struct{}, limit)
		s.warmupSems[slot] = sem
	}
	return sem
}

func authWarmupSemaphoresBusy(sems map[string]chan struct{}) bool {
	for _, sem := range sems {
		if len(sem) > 0 {
			return true
		}
	}
	return false
}

// WarmupAuthQuota runs one explicit operator-triggered quota-window warmup.
func (s *Service) WarmupAuthQuota(ctx context.Context, provider, authID, authIndex string, models []string) (AuthQuotaOverviewItem, error) {
	if s == nil {
		return AuthQuotaOverviewItem{}, errors.New("auth quota warmup unavailable")
	}
	models = uniqueAuthWarmupModels(models)
	if len(models) == 0 {
		return AuthQuotaOverviewItem{}, errors.New("manual warmup requires at least one model supported by this auth file")
	}
	schedule := store.AuthWarmupSchedule{ID: "manual", Name: "立即预热", Auths: []store.AuthWarmupAuthTarget{{Provider: provider, AuthID: authID, AuthIndex: authIndex}}, Models: models}
	// A manual button press is explicit operator intent. Unlike scheduled work,
	// it must not be suppressed by the day's prior manual warmup record.
	return s.warmupAuthQuota(ctx, "", provider, authID, authIndex, "manual:"+newAuthWarmupID(), schedule)
}

func (s *Service) warmupAuthQuota(ctx context.Context, callback, provider, authID, authIndex, scheduleKey string, schedule store.AuthWarmupSchedule) (AuthQuotaOverviewItem, error) {
	if s == nil {
		return AuthQuotaOverviewItem{}, errors.New("auth quota warmup unavailable")
	}
	executor := s.authWarmupExecutorValue()
	if executor == nil {
		return AuthQuotaOverviewItem{}, errors.New("auth quota warmup executor unavailable")
	}
	item, err := s.RefreshAuthQuota(ctx, callback, provider, authID, authIndex)
	if err != nil {
		return AuthQuotaOverviewItem{}, err
	}
	if item.Status != "fresh" {
		return item, errors.New("auth quota warmup requires a fresh quota snapshot")
	}
	identity := store.AuthIdentity{AuthID: item.AuthID, AuthIndex: item.AuthIndex, Provider: item.Provider, Name: item.DisplayName}
	models := uniqueAuthWarmupModels(schedule.Models)
	// One model per warmup. Requesting every model the account offers sends one
	// upstream call per model to every selected account at once, and that burst is
	// what makes the provider answer 502/overloaded to several accounts together.
	if selected := pickAuthWarmupModel(models); selected != "" {
		models = []string{selected}
	}
	if len(models) == 0 {
		return item, errors.New("auth quota warmup has no selected model")
	}
	run := store.AuthWarmupRun{
		ID:          newAuthWarmupID(),
		Provider:    item.Provider,
		AuthID:      item.AuthID,
		AuthIndex:   item.AuthIndex,
		Model:       models[0],
		ScheduleKey: scheduleKey,
		StartedAt:   time.Now().UTC(),
	}
	if err := s.AcquireAuthWarmup(ctx, run.ID, identity); err != nil {
		return s.attachAuthWarmupRun(warmupStoreContext(ctx), item), nil
	}
	defer s.ReleaseAuthWarmup(run.ID)
	storeCtx := warmupStoreContext(ctx)
	claimed, err := s.store.StartAuthWarmupRun(storeCtx, run)
	if err != nil {
		return item, err
	}
	if !claimed {
		s.disableOnceAuthWarmupSchedule(storeCtx, schedule)
		return s.attachAuthWarmupRun(storeCtx, item), nil
	}
	defer s.disableOnceAuthWarmupSchedule(storeCtx, schedule)

	result, execErr := executor.ExecuteAuthWarmup(ctx, AuthWarmupRequest{
		RunID:         run.ID,
		Auth:          identity,
		Models:        models,
		EntryProtocol: "openai",
		RequestedAt:   run.StartedAt,
	})
	if execErr != nil {
		status := "failed"
		if errors.Is(execErr, ErrAuthWarmupModelUnavailable) {
			status = "skipped"
		}
		_ = s.store.FinishAuthWarmupRun(storeCtx, run.ID, status, "", 0, 0, false, authWarmupProviderErrorCode(item.Provider, execErr))
		return s.attachAuthWarmupRun(storeCtx, item), nil
	}
	after, refreshErr := s.RefreshAuthQuota(ctx, callback, item.Provider, item.AuthID, item.AuthIndex)
	observed := refreshErr == nil && authQuotaWindowsChanged(item.Windows, after.Windows)
	_ = s.store.FinishAuthWarmupRun(storeCtx, run.ID, "succeeded", result.Model, result.Usage.Input, result.Usage.Output, observed, "")
	if refreshErr != nil {
		after = item
	}
	return s.attachAuthWarmupRun(storeCtx, after), nil
}

func warmupStoreContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

var ErrAuthWarmupModelUnavailable = errors.New("warmup target does not support a selected model")

// authWarmupPreferredModel is the model a warmup uses when the account offers it.
// Any single model refreshes the quota window, so the preferred one keeps warmups
// comparable across accounts instead of depending on the account's model order.
const authWarmupPreferredModel = "gpt-5.6-sol"

// pickAuthWarmupModel narrows a warmup model list to the one model that will be
// requested: the preferred model when the account offers it, otherwise the first
// model that is not image-only. It returns "" when nothing is warmable, and the
// caller then keeps the original list so the run is recorded as unsupported.
func pickAuthWarmupModel(models []string) string {
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model), authWarmupPreferredModel) {
			return strings.TrimSpace(model)
		}
	}
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" && !IsImageOnlyModel(model) {
			return model
		}
	}
	return ""
}

// AuthWarmupUpstreamStatusError carries the HTTP status a warmup request received
// from the host. Without it every unrecognised status collapses into
// execution_failed, which hides whether the host refused the request before it
// reached any account or the account itself answered badly.
type AuthWarmupUpstreamStatusError struct {
	Status  int
	Message string
}

func (e *AuthWarmupUpstreamStatusError) Error() string {
	if e == nil {
		return ""
	}
	if message := strings.TrimSpace(e.Message); message != "" {
		return message
	}
	return fmt.Sprintf("warmup request returned status %d", e.Status)
}

func (s *Service) disableOnceAuthWarmupSchedule(ctx context.Context, schedule store.AuthWarmupSchedule) {
	if s == nil || s.store == nil || store.AuthWarmupFrequency(schedule.Frequency) != store.AuthWarmupFrequencyOnce {
		return
	}
	scheduleID := strings.TrimSpace(schedule.ID)
	if scheduleID == "" || scheduleID == "manual" {
		return
	}
	settings, err := s.store.GetAuthWarmupSettings(ctx)
	if err != nil {
		return
	}
	changed := false
	for i := range settings.Schedules {
		if settings.Schedules[i].ID != scheduleID || !settings.Schedules[i].Enabled {
			continue
		}
		settings.Schedules[i].Enabled = false
		changed = true
	}
	if !changed {
		return
	}
	_, _ = s.store.UpsertAuthWarmupSettings(ctx, settings)
}

func uniqueAuthWarmupModels(models []string) []string {
	unique := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := unique[model]; exists {
			continue
		}
		unique[model] = struct{}{}
		result = append(result, model)
	}
	return result
}

func shuffleAuthWarmupModels(models []string) []string {
	models = uniqueAuthWarmupModels(models)
	for index := len(models) - 1; index > 0; index-- {
		pick, err := rand.Int(rand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			pick = big.NewInt(time.Now().UnixNano() % int64(index+1))
		}
		models[index], models[pick.Int64()] = models[pick.Int64()], models[index]
	}
	return models
}

func authWarmupIsDue(settings store.AuthWarmupSettings, schedule store.AuthWarmupSchedule, file AuthQuotaFile, now time.Time) bool {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return false
	}
	warmupHour, warmupMinute, err := parseWarmupClock(schedule.WarmupAt)
	if err != nil {
		return false
	}
	localNow := now.In(location)
	freq := store.AuthWarmupFrequency(schedule.Frequency)
	if freq == store.AuthWarmupFrequencyWeekly && !store.AuthWarmupMatchesWeekday(schedule, localNow.Weekday()) {
		return false
	}
	if freq == store.AuthWarmupFrequencyOnce {
		on, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(schedule.WarmupOn), location)
		if err != nil || localNow.Year() != on.Year() || localNow.YearDay() != on.YearDay() {
			return false
		}
	}
	due := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), warmupHour, warmupMinute, 0, 0, location).Add(authWarmupJitter(time.Duration(settings.StableJitterSeconds)*time.Second, file))
	return !localNow.Before(due) && localNow.Before(due.Add(15*time.Minute))
}

func scheduledWarmupKey(schedule store.AuthWarmupSchedule, now time.Time) string {
	warmupAt := strings.TrimSpace(schedule.WarmupAt)
	if parsed, err := time.Parse("15:04", warmupAt); err == nil {
		warmupAt = parsed.Format("15:04")
	}
	location, err := time.LoadLocation(strings.TrimSpace(schedule.Timezone))
	freq := store.AuthWarmupFrequency(schedule.Frequency)
	day := now.UTC().Format("2006-01-02")
	if err == nil {
		day = now.In(location).Format("2006-01-02")
	}
	if freq == store.AuthWarmupFrequencyOnce {
		if on := strings.TrimSpace(schedule.WarmupOn); on != "" {
			day = on
		}
	}
	return "scheduled:" + schedule.ID + ":" + freq + ":" + day + ":" + warmupAt
}

func findWarmupAuthFile(files []AuthQuotaFile, target store.AuthWarmupAuthTarget) (AuthQuotaFile, bool) {
	for _, file := range files {
		if strings.TrimSpace(target.AuthIndex) != "" && strings.TrimSpace(target.AuthIndex) == strings.TrimSpace(file.AuthIndex) {
			return file, true
		}
		if strings.TrimSpace(target.AuthID) != "" && (strings.TrimSpace(target.AuthID) == strings.TrimSpace(file.ID) || strings.TrimSpace(target.AuthID) == strings.TrimSpace(file.Name)) {
			return file, true
		}
	}
	return AuthQuotaFile{}, false
}

func parseWarmupClock(value string) (int, int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, 0, err
	}
	return parsed.Hour(), parsed.Minute(), nil
}

func authWarmupJitter(max time.Duration, file AuthQuotaFile) time.Duration {
	if max <= 0 {
		return 0
	}
	key := []byte(first(file.ID, file.AuthIndex, file.Name))
	var sum uint64
	for _, value := range key {
		sum = sum*131 + uint64(value)
	}
	return time.Duration(sum%uint64(max/time.Second+1)) * time.Second
}

// authQuotaWindowsChanged confirms that the post-warmup upstream snapshot
// materially changed. It works for short and long rolling windows alike.
func authQuotaWindowsChanged(before, after []AuthQuotaWindow) bool {
	previous := make(map[string]AuthQuotaWindow, len(before))
	for _, window := range before {
		previous[authQuotaWindowKey(window)] = window
	}
	for _, current := range after {
		prior, exists := previous[authQuotaWindowKey(current)]
		if !exists {
			return true
		}
		if !sameAuthQuotaTime(prior.CycleStartAt, current.CycleStartAt) || !sameAuthQuotaTime(prior.ResetsAt, current.ResetsAt) || !sameAuthQuotaFloat(prior.Used, current.Used) || !sameAuthQuotaFloat(prior.Remaining, current.Remaining) {
			return true
		}
	}
	return false
}

func authQuotaWindowKey(window AuthQuotaWindow) string {
	return strings.TrimSpace(window.ID) + "|" + strings.TrimSpace(window.Scope) + "|" + strings.TrimSpace(window.ScopeID)
}

func sameAuthQuotaTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.UTC().Equal(right.UTC())
}

func sameAuthQuotaFloat(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return math.Abs(*left-*right) < 1e-9
}

func newAuthWarmupID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("warmup-%d", time.Now().UnixNano())
	}
	return "warmup-" + hex.EncodeToString(buf)
}

func authWarmupErrorCode(err error) string {
	message := ""
	if err != nil {
		message = strings.ToLower(err.Error())
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrAuthWarmupModelUnavailable):
		return "unsupported_model"
	case strings.Contains(message, "missing api key"):
		return "missing_api_key"
	case strings.Contains(message, "unauthorized"), strings.Contains(message, "401"):
		return "unauthorized"
	case strings.Contains(message, "rate limit"), strings.Contains(message, "429"):
		return "rate_limited"
	case strings.Contains(message, "target unavailable"):
		return "target_unavailable"
	}
	var statusErr *AuthWarmupUpstreamStatusError
	if errors.As(err, &statusErr) && statusErr != nil && statusErr.Status > 0 {
		return fmt.Sprintf("status_%d", statusErr.Status)
	}
	return "execution_failed"
}

func authWarmupProviderErrorCode(provider string, err error) string {
	code := authWarmupErrorCode(err)
	// CPA's nested host.model.execute callback intentionally strips opaque
	// upstream bodies. XAI OAuth failures therefore arrive as a generic model
	// execution error, even though the direct provider response is 401 Missing API key.
	if code == "execution_failed" && quotaProvider(provider) == "xai" {
		return "xai_auth_required"
	}
	return code
}

func (s *Service) AcquireAuthWarmup(ctx context.Context, runID string, auth store.AuthIdentity) error {
	if s == nil || strings.TrimSpace(runID) == "" || auth.Empty() {
		return store.ErrInvalidArgument
	}
	provider, authID := authLimitIdentity(auth)
	if provider == "" || authID == "" {
		return store.ErrInvalidArgument
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	if s.warmupHolds == nil {
		s.warmupHolds = make(map[string]store.AuthIdentity)
	}
	if s.authWarmupBusyLocked(provider, authID) || s.activeAuthRequestsLocked(provider, authID, "") > 0 {
		return store.ErrConcurrentLimit
	}
	s.warmupHolds[runID] = auth
	return nil
}

func (s *Service) ReleaseAuthWarmup(runID string) {
	if s == nil {
		return
	}
	s.authMu.Lock()
	delete(s.warmupHolds, strings.TrimSpace(runID))
	s.authMu.Unlock()
}
