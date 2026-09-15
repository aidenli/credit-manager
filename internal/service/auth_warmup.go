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
	sem := s.authWarmupSemaphore(parallel)
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
			select {
			case sem <- struct{}{}:
			default:
				// A later ticker iteration can catch this task while it remains
				// inside the bounded catch-up window below.
				continue
			}
			go func() {
				defer func() { <-sem }()
				// The task/date key is durable idempotency for each selected auth.
				_, _ = s.warmupAuthQuota(ctx, "", file.Provider, first(file.ID, file.Name, file.AuthIndex), file.AuthIndex, scheduledWarmupKey(schedule, now), true, settings, schedule)
			}()
		}
	}
}

func (s *Service) authWarmupSemaphore(limit int) chan struct{} {
	if limit < 1 {
		limit = 1
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	if s.warmupSem == nil || (s.warmupSemLimit != limit && len(s.warmupSem) == 0) {
		s.warmupSem = make(chan struct{}, limit)
		s.warmupSemLimit = limit
	}
	return s.warmupSem
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
	settings, err := s.store.GetAuthWarmupSettings(ctx)
	if err != nil {
		return AuthQuotaOverviewItem{}, err
	}
	schedule := store.AuthWarmupSchedule{ID: "manual", Name: "立即预热", Auths: []store.AuthWarmupAuthTarget{{Provider: provider, AuthID: authID, AuthIndex: authIndex}}, Models: models}
	// A manual button press is explicit operator intent. Unlike scheduled work,
	// it must not be suppressed by the day's prior manual warmup record.
	return s.warmupAuthQuota(ctx, "", provider, authID, authIndex, "manual:"+newAuthWarmupID(), false, settings, schedule)
}

func (s *Service) warmupAuthQuota(ctx context.Context, callback, provider, authID, authIndex, scheduleKey string, skipActiveWindow bool, settings store.AuthWarmupSettings, schedule store.AuthWarmupSchedule) (AuthQuotaOverviewItem, error) {
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
	if skipActiveWindow && authQuotaHasActiveShortWindow(item, time.Now()) {
		return s.attachAuthWarmupRun(warmupStoreContext(ctx), item), nil
	}
	identity := store.AuthIdentity{AuthID: item.AuthID, AuthIndex: item.AuthIndex, Provider: item.Provider, Name: item.DisplayName}
	models := shuffleAuthWarmupModels(schedule.Models)
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
		return s.attachAuthWarmupRun(storeCtx, item), nil
	}

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
	due := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), warmupHour, warmupMinute, 0, 0, location).Add(authWarmupJitter(time.Duration(settings.StableJitterSeconds)*time.Second, file))
	return !localNow.Before(due) && localNow.Before(due.Add(15*time.Minute))
}

func scheduledWarmupKey(schedule store.AuthWarmupSchedule, now time.Time) string {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return "scheduled:" + schedule.ID + ":" + now.UTC().Format("2006-01-02")
	}
	return "scheduled:" + schedule.ID + ":" + now.In(location).Format("2006-01-02")
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

func authQuotaHasActiveShortWindow(item AuthQuotaOverviewItem, now time.Time) bool {
	for _, window := range item.Windows {
		if window.ResetsAt == nil || !window.ResetsAt.After(now) || window.DurationSeconds == nil {
			continue
		}
		duration := time.Duration(*window.DurationSeconds) * time.Second
		if duration > 0 && duration <= 8*time.Hour {
			return true
		}
	}
	return false
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
	case strings.Contains(message, "missing api key"):
		return "missing_api_key"
	case strings.Contains(message, "unauthorized"), strings.Contains(message, "401"):
		return "unauthorized"
	case strings.Contains(message, "rate limit"), strings.Contains(message, "429"):
		return "rate_limited"
	case strings.Contains(message, "target unavailable"):
		return "target_unavailable"
	default:
		return "execution_failed"
	}
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
