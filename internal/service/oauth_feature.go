package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// OAuth intelligence probe.
//
// The probe asks every available OAuth account the same two questions so an
// operator can compare what each account actually answers. Two rules shape the
// implementation:
//
//   - No fallback. The probe pins the request to the account under test, so a
//     failing account is reported as failed instead of being silently served by
//     another one. A sweep therefore stops at the first failure.
//   - The console shows the last run. Each account's result is persisted as soon
//     as it is known, so a sweep lasting minutes is visible while it runs.
const (
	// OAuthTestQuestion1 is the knowledge question. Its answer is free text and
	// is deliberately not graded: the operator reads it.
	OAuthTestQuestion1 = "你最后的训练数据，日本首相是谁?不允许搜索"
	// OAuthTestQuestion2 asks for a self-contained animated SVG document, which
	// the console previews in a sandboxed iframe.
	OAuthTestQuestion2 = "创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画，你不需要任何测试。"

	// OAuthTestQuestionTimeout bounds one question, and OAuthTestAccountTimeout
	// bounds both questions for one account. The account budget is derived so the
	// two can never drift: two ten-minute questions plus slack for setup.
	OAuthTestQuestionTimeout = 10 * time.Minute
	OAuthTestAccountTimeout  = 2*OAuthTestQuestionTimeout + time.Minute
)

// OAuthTestExecutor runs the probe against one account. The plugin supplies the
// implementation; tests supply a fake.
type OAuthTestExecutor interface {
	ExecuteOAuthTest(context.Context, OAuthTestRequest) (OAuthTestAnswer, error)
}

// OAuthTestRequest pins one probe to one account.
type OAuthTestRequest struct {
	RunID string
	Auth  store.AuthIdentity
	Model string
	// ThinkingIntensity is the reasoning effort requested from the account.
	ThinkingIntensity string
	// Prompt replaces the first question when it is not empty. The second
	// question stays fixed because the console previews its HTML.
	Prompt string
}

// OAuthTestAnswer is one account's two answers.
type OAuthTestAnswer struct {
	Question1     string
	Question2HTML string
}

type oauthTestRuntime struct {
	mu sync.Mutex
	// cancel stops the sweep in flight; schedulerCancel stops the ticker that
	// starts sweeps. They are separate on purpose: changing the schedule must
	// never kill a running sweep, and stopping a sweep must never disable the
	// schedule.
	cancel          context.CancelFunc
	schedulerCancel context.CancelFunc
	running         bool
	executor        OAuthTestExecutor
}

// SetOAuthTestExecutor attaches the plugin-side probe implementation.
func (s *Service) SetOAuthTestExecutor(executor OAuthTestExecutor) {
	if s == nil {
		return
	}
	s.oauthTest.mu.Lock()
	s.oauthTest.executor = executor
	s.oauthTest.mu.Unlock()
}

func (s *Service) oauthTestExecutor() OAuthTestExecutor {
	if s == nil {
		return nil
	}
	s.oauthTest.mu.Lock()
	defer s.oauthTest.mu.Unlock()
	return s.oauthTest.executor
}

// OAuthTestSettings returns the persisted schedule.
func (s *Service) OAuthTestSettings(ctx context.Context) (store.OAuthTestSettings, error) {
	return s.store.GetOAuthTestSettings(ctx)
}

// UpdateOAuthTestSettings stores the schedule and applies it to the running
// scheduler immediately, so the console does not need a plugin reload.
func (s *Service) UpdateOAuthTestSettings(ctx context.Context, settings store.OAuthTestSettings) (store.OAuthTestSettings, error) {
	updated, err := s.store.UpsertOAuthTestSettings(ctx, settings)
	if err != nil {
		return store.OAuthTestSettings{}, err
	}
	s.RestartOAuthTestScheduler()
	return updated, nil
}

// StartOAuthTest begins a sweep in the background and returns the run it created.
func (s *Service) StartOAuthTest(ctx context.Context) (store.OAuthTestRun, error) {
	s.oauthTest.mu.Lock()
	if s.oauthTest.running {
		s.oauthTest.mu.Unlock()
		return store.OAuthTestRun{}, errors.New("oauth test is already running")
	}
	executor := s.oauthTest.executor
	if executor == nil {
		s.oauthTest.mu.Unlock()
		return store.OAuthTestRun{}, errors.New("oauth test executor unavailable")
	}
	s.oauthTest.running = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.oauthTest.cancel = cancel
	s.oauthTest.mu.Unlock()

	settings, err := s.store.GetOAuthTestSettings(ctx)
	if err != nil {
		cancel()
		s.finishOAuthTestRuntime()
		return store.OAuthTestRun{}, err
	}
	run := store.OAuthTestRun{
		ID:                fmt.Sprintf("oauth-test-%d", time.Now().UnixNano()),
		Status:            "running",
		StartedAt:         time.Now().UTC(),
		Model:             settings.Model,
		ThinkingIntensity: settings.ThinkingIntensity,
		Prompt:            settings.Prompt,
		Results:           []store.OAuthTestResult{},
	}
	if err := s.store.SaveOAuthTestRun(ctx, run); err != nil {
		cancel()
		s.finishOAuthTestRuntime()
		return store.OAuthTestRun{}, err
	}
	go s.runOAuthTest(runCtx, run, settings, executor)
	return run, nil
}

// runOAuthTest sweeps the available accounts in a stable order and stops at the
// first failure, because a fallback would measure a different account than the
// one under test.
func (s *Service) runOAuthTest(ctx context.Context, run store.OAuthTestRun, settings store.OAuthTestSettings, executor OAuthTestExecutor) {
	accounts, err := s.AuthAccounts(ctx)
	if err != nil {
		run.Status, run.Error = "failed", err.Error()
		s.completeOAuthTest(run)
		return
	}
	available := make([]AuthAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.OAuth && !account.Disabled {
			available = append(available, account)
		}
	}
	sort.Slice(available, func(i, j int) bool {
		if available[i].Provider == available[j].Provider {
			return available[i].ID < available[j].ID
		}
		return available[i].Provider < available[j].Provider
	})
	if len(available) == 0 {
		run.Status, run.Error = "failed", "没有可用的 OAuth 账号"
		s.completeOAuthTest(run)
		return
	}

	// Every account is probed at the same time, and every account's outcome is
	// recorded: a failure here is data, not a reason to abandon the sweep. The
	// results slice is indexed by the account order so the console keeps a stable
	// list while the probes finish out of order.
	results := make([]store.OAuthTestResult, len(available))
	started := time.Now().UTC()
	for index, account := range available {
		results[index] = store.OAuthTestResult{
			Provider:    account.Provider,
			AuthID:      account.ID,
			AuthIndex:   account.AuthIndex,
			DisplayName: account.DisplayName,
			Status:      "running",
			StartedAt:   started,
		}
	}
	run.Results = results
	s.saveOAuthTestProgress(run)

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		failed int
	)
	for index, account := range available {
		wg.Add(1)
		go func(index int, account AuthAccount) {
			defer wg.Done()
			result := results[index]
			accountCtx, cancel := context.WithTimeout(ctx, OAuthTestAccountTimeout)
			defer cancel()
			answer, callErr := executor.ExecuteOAuthTest(accountCtx, OAuthTestRequest{
				RunID:             run.ID,
				Auth:              store.AuthIdentity{AuthID: account.ID, AuthIndex: account.AuthIndex, Provider: account.Provider, Name: account.DisplayName},
				Model:             run.Model,
				ThinkingIntensity: run.ThinkingIntensity,
				Prompt:            settings.Prompt,
			})
			done := time.Now().UTC()
			result.CompletedAt = &done
			if callErr != nil {
				failure := callErr
				switch {
				case errors.Is(accountCtx.Err(), context.DeadlineExceeded):
					failure = fmt.Errorf("超时（%s）: %w", OAuthTestAccountTimeout, callErr)
				case errors.Is(ctx.Err(), context.Canceled):
					failure = fmt.Errorf("已停止: %w", callErr)
				}
				result.Status = "failed"
				result.Error = failure.Error()
			} else {
				result.Status = "succeeded"
				result.Question1 = answer.Question1
				result.Question2HTML = answer.Question2HTML
			}

			mu.Lock()
			results[index] = result
			run.Results = results
			if result.Status != "succeeded" {
				failed++
			}
			s.saveOAuthTestProgress(run)
			mu.Unlock()
		}(index, account)
	}
	wg.Wait()

	switch {
	case ctx.Err() != nil:
		run.Status, run.Error = "stopped", "测试已停止"
	case failed > 0:
		run.Status = "failed"
		run.Error = fmt.Sprintf("%d/%d 个账号未通过（详见各账号错误）", failed, len(results))
	default:
		run.Status = "succeeded"
	}
	s.completeOAuthTest(run)
}

// saveOAuthTestProgress persists a run while it is still in flight. Failures are
// ignored on purpose: progress reporting must never abort a sweep.
func (s *Service) saveOAuthTestProgress(run store.OAuthTestRun) {
	_ = s.store.SaveOAuthTestRun(context.Background(), run)
}

func (s *Service) completeOAuthTest(run store.OAuthTestRun) {
	if run.CompletedAt == nil {
		now := time.Now().UTC()
		run.CompletedAt = &now
	}
	_ = s.store.SaveOAuthTestRun(context.Background(), run)
	s.finishOAuthTestRuntime()
}

func (s *Service) finishOAuthTestRuntime() {
	s.oauthTest.mu.Lock()
	s.oauthTest.running = false
	s.oauthTest.cancel = nil
	s.oauthTest.mu.Unlock()
}

// StopOAuthTest cancels the sweep in flight. The schedule keeps running.
func (s *Service) StopOAuthTest() {
	if s == nil {
		return
	}
	s.oauthTest.mu.Lock()
	cancel := s.oauthTest.cancel
	s.oauthTest.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// RestartOAuthTestScheduler replaces the ticker with one built from the current
// settings. It only touches the scheduler, never the sweep in flight.
func (s *Service) RestartOAuthTestScheduler() {
	if s == nil {
		return
	}
	s.oauthTest.mu.Lock()
	if s.oauthTest.schedulerCancel != nil {
		s.oauthTest.schedulerCancel()
		s.oauthTest.schedulerCancel = nil
	}
	s.oauthTest.mu.Unlock()

	settings, err := s.store.GetOAuthTestSettings(context.Background())
	if err != nil || !settings.Enabled || s.oauthTestExecutor() == nil {
		return
	}
	interval := time.Duration(settings.IntervalMinutes) * time.Minute
	if interval <= 0 {
		return
	}
	schedCtx, cancel := context.WithCancel(context.Background())
	s.oauthTest.mu.Lock()
	s.oauthTest.schedulerCancel = cancel
	s.oauthTest.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-schedCtx.Done():
				return
			case <-ticker.C:
				// A tick that arrives while a sweep is running is dropped: the
				// next tick is soon enough, and overlapping sweeps would fight
				// over the same accounts.
				_, _ = s.StartOAuthTest(context.Background())
			}
		}
	}()
}

// StartOAuthTestScheduler arms the periodic sweep from the stored settings.
func (s *Service) StartOAuthTestScheduler() { s.RestartOAuthTestScheduler() }

// StopOAuthTestScheduler disarms the periodic sweep and cancels the sweep in
// flight. It is called when the service closes.
func (s *Service) StopOAuthTestScheduler() {
	if s == nil {
		return
	}
	s.oauthTest.mu.Lock()
	cancelScheduler := s.oauthTest.schedulerCancel
	cancelRun := s.oauthTest.cancel
	s.oauthTest.schedulerCancel = nil
	s.oauthTest.mu.Unlock()
	if cancelScheduler != nil {
		cancelScheduler()
	}
	if cancelRun != nil {
		cancelRun()
	}
}

// LatestOAuthTest returns the most recent run, including one still in flight.
func (s *Service) LatestOAuthTest(ctx context.Context) (store.OAuthTestRun, error) {
	return s.store.GetLatestOAuthTestRun(ctx)
}

// MarkOAuthTestInterrupted closes a sweep the previous process left running.
func (s *Service) MarkOAuthTestInterrupted(ctx context.Context) error {
	return s.store.MarkOAuthTestInterrupted(ctx)
}

// OAuthTestQuestions returns the two questions a run asks. A custom prompt
// replaces the knowledge question; the SVG task stays because the console
// previews its HTML.
func OAuthTestQuestions(prompt string) (string, string) {
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		return trimmed, OAuthTestQuestion2
	}
	return OAuthTestQuestion1, OAuthTestQuestion2
}
