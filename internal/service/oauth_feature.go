package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// OAuth intelligence probe.
//
// The probe asks every available OAuth account the same two questions, on a daily
// schedule or on demand, so an operator can compare what each account answers.
// Three rules shape the implementation:
//
//   - No fallback. The probe pins the request to the account under test, so a
//     failing account is that account's failure and is never answered by another.
//   - Accounts are independent. They are probed at the same time, each result is
//     stored as soon as it is known, and a failure is recorded rather than ending
//     the sweep.
//   - The console is a card per account. Which is why results are stored per
//     account instead of per sweep.
const (
	// OAuthTestQuestion1 is the knowledge question. Its answer is free text and
	// deliberately not graded: the operator reads it.
	OAuthTestQuestion1 = "你最后的训练数据，日本首相是谁?不允许搜索"
	// OAuthTestQuestion2 asks for a self-contained animated SVG document, which
	// the console previews in a sandboxed iframe.
	OAuthTestQuestion2 = "创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画，你不需要任何测试。"

	// OAuthTestQuestionTimeout bounds one question. It lives here because the
	// plugin enforces it and the README documents it.
	OAuthTestQuestionTimeout = 10 * time.Minute
)

// OAuthTestQuestions returns the two questions the probe asks. They are fixed on
// purpose: the console compares accounts, so every account must be asked the same
// thing.
func OAuthTestQuestions() (string, string) { return OAuthTestQuestion1, OAuthTestQuestion2 }

// OAuthTestExecutor runs both questions against one account.
type OAuthTestExecutor interface {
	ExecuteOAuthTest(context.Context, OAuthTestRequest) (OAuthTestAnswer, error)
}

// OAuthTestRequest pins one probe to one account.
type OAuthTestRequest struct {
	Auth              store.AuthIdentity
	Model             string
	ThinkingIntensity string
}

// OAuthTestAnswer is one account's two answers.
type OAuthTestAnswer struct {
	Question1     string
	Question2HTML string
}

// OAuthTestAccount is one card in the console: the account plus its latest probe
// outcome and whether a probe is running right now.
type OAuthTestAccount struct {
	Provider    string                 `json:"provider"`
	AuthID      string                 `json:"auth_id"`
	AuthIndex   string                 `json:"auth_index,omitempty"`
	DisplayName string                 `json:"display_name"`
	Status      string                 `json:"status"`
	Running     bool                   `json:"running"`
	Result      *store.OAuthTestResult `json:"result,omitempty"`
}

type oauthTestRuntime struct {
	mu sync.Mutex
	// executor is the plugin-side probe implementation.
	executor OAuthTestExecutor
	// schedulerCancel stops the daily timer; inFlight holds one cancel per account
	// being probed. Restarting the schedule must never touch a probe in flight,
	// which is why the two are separate.
	schedulerCancel context.CancelFunc
	inFlight        map[string]context.CancelFunc
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

// UpdateOAuthTestSettings stores the schedule and applies it immediately, so the
// console does not need a plugin reload.
func (s *Service) UpdateOAuthTestSettings(ctx context.Context, settings store.OAuthTestSettings) (store.OAuthTestSettings, error) {
	updated, err := s.store.UpsertOAuthTestSettings(ctx, settings)
	if err != nil {
		return store.OAuthTestSettings{}, err
	}
	s.RestartOAuthTestScheduler()
	return updated, nil
}

// OAuthTestAccounts lists every OAuth account as a console card, each carrying
// its latest stored result and whether it is being probed right now.
func (s *Service) OAuthTestAccounts(ctx context.Context) ([]OAuthTestAccount, error) {
	accounts, err := s.availableOAuthTestAccounts(ctx)
	if err != nil {
		return nil, err
	}
	results, err := s.store.GetOAuthTestResults(ctx)
	if err != nil {
		return nil, err
	}
	s.oauthTest.mu.Lock()
	running := make(map[string]bool, len(s.oauthTest.inFlight))
	for authID := range s.oauthTest.inFlight {
		running[authID] = true
	}
	s.oauthTest.mu.Unlock()

	cards := make([]OAuthTestAccount, 0, len(accounts))
	for _, account := range accounts {
		card := OAuthTestAccount{
			Provider:    account.Provider,
			AuthID:      account.ID,
			AuthIndex:   account.AuthIndex,
			DisplayName: account.DisplayName,
			Status:      "untested",
			Running:     running[strings.TrimSpace(account.ID)],
		}
		if stored, ok := results[strings.TrimSpace(account.ID)]; ok {
			result := stored
			card.Result = &result
			card.Status = stored.Status
		}
		if card.Running {
			card.Status = "running"
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// availableOAuthTestAccounts returns the probeable accounts in a stable order.
func (s *Service) availableOAuthTestAccounts(ctx context.Context) ([]AuthAccount, error) {
	accounts, err := s.AuthAccounts(ctx)
	if err != nil {
		return nil, err
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
	return available, nil
}

// StartOAuthTest probes every available account, or just authID when it is set.
// It returns how many probes it started; an account already being probed is
// skipped in a sweep and rejected when it was asked for by name.
func (s *Service) StartOAuthTest(ctx context.Context, authID string) (int, error) {
	executor := s.oauthTestExecutor()
	if executor == nil {
		return 0, errors.New("oauth test executor unavailable")
	}
	available, err := s.availableOAuthTestAccounts(ctx)
	if err != nil {
		return 0, err
	}
	settings, err := s.store.GetOAuthTestSettings(ctx)
	if err != nil {
		return 0, err
	}
	target := strings.TrimSpace(authID)
	if target != "" {
		found := false
		for _, account := range available {
			if strings.TrimSpace(account.ID) == target {
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("账号不可测试: %s", target)
		}
	}

	started := 0
	for _, account := range available {
		accountID := strings.TrimSpace(account.ID)
		if target != "" && accountID != target {
			continue
		}
		s.oauthTest.mu.Lock()
		if s.oauthTest.inFlight == nil {
			s.oauthTest.inFlight = map[string]context.CancelFunc{}
		}
		if _, running := s.oauthTest.inFlight[accountID]; running {
			s.oauthTest.mu.Unlock()
			if target != "" {
				return started, fmt.Errorf("该账号正在测试中: %s", account.DisplayName)
			}
			continue
		}
		probeCtx, cancel := context.WithCancel(context.Background())
		s.oauthTest.inFlight[accountID] = cancel
		s.oauthTest.mu.Unlock()

		started++
		go s.probeOAuthTestAccount(probeCtx, account, settings, executor)
	}
	if started == 0 {
		return 0, errors.New("没有可测试的账号")
	}
	return started, nil
}

// probeOAuthTestAccount asks one account both questions and stores the outcome.
func (s *Service) probeOAuthTestAccount(ctx context.Context, account AuthAccount, settings store.OAuthTestSettings, executor OAuthTestExecutor) {
	result := store.OAuthTestResult{
		Provider:    account.Provider,
		AuthID:      account.ID,
		AuthIndex:   account.AuthIndex,
		DisplayName: account.DisplayName,
		Status:      "succeeded",
		Model:       settings.Model,
		StartedAt:   time.Now().UTC(),
	}
	answer, err := executor.ExecuteOAuthTest(ctx, OAuthTestRequest{
		Auth:              store.AuthIdentity{AuthID: account.ID, AuthIndex: account.AuthIndex, Provider: account.Provider, Name: account.DisplayName},
		Model:             settings.Model,
		ThinkingIntensity: settings.ThinkingIntensity,
	})
	if err != nil {
		failure := err
		// A cancelled context is the operator stopping the probe, not the account
		// failing: say so, because the card keeps this text until the next run.
		if errors.Is(ctx.Err(), context.Canceled) {
			failure = fmt.Errorf("已停止: %w", err)
		}
		result.Status = "failed"
		result.Error = failure.Error()
	} else {
		result.Question1 = answer.Question1
		result.Question2HTML = answer.Question2HTML
		result.Question2File = s.writeOAuthTestHTML(account.ID, result.StartedAt, answer.Question2HTML)
	}
	done := time.Now().UTC()
	result.CompletedAt = &done

	// The result is stored even while other accounts are still running, and the
	// account leaves the in-flight set only after its card can show the outcome.
	_ = s.store.SaveOAuthTestResult(context.Background(), result)
	s.oauthTest.mu.Lock()
	delete(s.oauthTest.inFlight, strings.TrimSpace(account.ID))
	s.oauthTest.mu.Unlock()
}

// StopOAuthTest cancels every probe in flight. The daily schedule keeps running.
func (s *Service) StopOAuthTest() {
	if s == nil {
		return
	}
	s.oauthTest.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.oauthTest.inFlight))
	for _, cancel := range s.oauthTest.inFlight {
		cancels = append(cancels, cancel)
	}
	s.oauthTest.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// RestartOAuthTestScheduler replaces the daily timer with one built from the
// current settings. It never touches a probe in flight.
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
	schedCtx, cancel := context.WithCancel(context.Background())
	s.oauthTest.mu.Lock()
	s.oauthTest.schedulerCancel = cancel
	s.oauthTest.mu.Unlock()

	hour, minute := settings.HourUTC, settings.MinuteUTC
	go func() {
		for {
			wait := time.Until(NextOAuthTestRun(time.Now(), hour, minute))
			timer := time.NewTimer(wait)
			select {
			case <-schedCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			// The sweep is skipped when the previous one is still running: the
			// next daily slot is soon enough, and overlapping sweeps would probe
			// the same accounts twice.
			_, _ = s.StartOAuthTest(context.Background(), "")
		}
	}()
}

// StartOAuthTestScheduler arms the daily probe from the stored settings.
func (s *Service) StartOAuthTestScheduler() { s.RestartOAuthTestScheduler() }

// StopOAuthTestScheduler disarms the daily probe and cancels every probe in
// flight. It is called when the service closes.
func (s *Service) StopOAuthTestScheduler() {
	if s == nil {
		return
	}
	s.oauthTest.mu.Lock()
	cancelScheduler := s.oauthTest.schedulerCancel
	s.oauthTest.schedulerCancel = nil
	s.oauthTest.mu.Unlock()
	if cancelScheduler != nil {
		cancelScheduler()
	}
	s.StopOAuthTest()
}

// NextOAuthTestRun returns the next UTC instant matching hour:minute. The time is
// interpreted in UTC because the console converts the operator's local time
// before saving, so the schedule never depends on the server's timezone.
func NextOAuthTestRun(now time.Time, hour, minute int) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// writeOAuthTestHTML stores the extracted document under the plugin data
// directory so an operator can open or share the exact file the card previews.
// It is best effort: the console renders the copy kept in the result, so a failed
// write must never turn a successful probe into a failure.
func (s *Service) writeOAuthTestHTML(authID string, started time.Time, html string) string {
	if s == nil || s.cfg.DataDir == "" || strings.TrimSpace(html) == "" {
		return ""
	}
	dir := filepath.Join(s.cfg.DataDir, "oauth-tests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-%s.html", oauthTestFileStem(authID), started.UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(html), 0o600); err != nil {
		return ""
	}
	return path
}

// oauthTestFileStem turns an auth id into a filename-safe stem: auth ids contain
// colons and slashes, which must never escape the data directory.
func oauthTestFileStem(authID string) string {
	stem := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(authID))
	stem = strings.Trim(stem, "-")
	if stem == "" {
		stem = "account"
	}
	if len(stem) > 64 {
		stem = stem[:64]
	}
	return stem
}
