package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// fakeOAuthTestExecutor records what the probe asked for, and can fail, block, or
// hold a barrier until several accounts are in flight at once.
type fakeOAuthTestExecutor struct {
	mu          sync.Mutex
	calls       []OAuthTestRequest
	inFlight    int
	maxInFlight int
	failOn      string
	block       chan struct{}
	barrier     int
	reached     chan struct{}
}

func (f *fakeOAuthTestExecutor) ExecuteOAuthTest(ctx context.Context, request OAuthTestRequest) (OAuthTestAnswer, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	if f.barrier > 0 && f.inFlight >= f.barrier && f.reached != nil {
		close(f.reached)
		f.reached = nil
	}
	block, failOn := f.block, f.failOn
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return OAuthTestAnswer{}, ctx.Err()
		}
	}
	if failOn != "" && request.Auth.AuthID == failOn {
		return OAuthTestAnswer{}, errors.New("上游返回 HTTP 503")
	}
	return OAuthTestAnswer{Question1: "答案 " + request.Auth.AuthID, Question2HTML: "<svg></svg>"}, nil
}

func (f *fakeOAuthTestExecutor) recorded() []OAuthTestRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]OAuthTestRequest(nil), f.calls...)
}

func (f *fakeOAuthTestExecutor) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

func oauthTestAccounts() *fakeQuotaSource {
	return &fakeQuotaSource{
		files: []AuthQuotaFile{
			{ID: "codex-a@example.com-pro.json", AuthIndex: "idx-a", Provider: "codex", Type: "codex", Email: "a@example.com"},
			{ID: "codex-b@example.com-pro.json", AuthIndex: "idx-b", Provider: "codex", Type: "codex", Email: "b@example.com"},
			{ID: "codex-c@example.com-pro.json", AuthIndex: "idx-c", Provider: "codex", Type: "codex", Email: "c@example.com"},
			{ID: "openai-compatibility:deepseek:1", AuthIndex: "idx-api", Provider: "openai-compatible-deepseek", Type: "openai-compatibility", Label: "deepseek"},
		},
		auths: map[string]string{
			"idx-a":   `{"access_token":"oauth-a","email":"a@example.com"}`,
			"idx-b":   `{"access_token":"oauth-b","email":"b@example.com"}`,
			"idx-c":   `{"access_token":"oauth-c","email":"c@example.com"}`,
			"idx-api": `{"api_key":"sk-secret"}`,
		},
	}
}

// waitOAuthTestIdle waits until no probe is running.
func waitOAuthTestIdle(t *testing.T, s *Service) []OAuthTestAccount {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cards, err := s.OAuthTestAccounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, card := range cards {
			if card.Running {
				running++
			}
		}
		if running == 0 {
			return cards
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("probes did not settle")
	return nil
}

// The daily schedule fires at the requested UTC time, and rolls to tomorrow once
// today's slot has passed.
func TestNextOAuthTestRunPicksTodayOrTomorrow(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)
	if got := NextOAuthTestRun(now, 9, 15); !got.Equal(time.Date(2026, 9, 25, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("later today = %s", got)
	}
	if got := NextOAuthTestRun(now, 8, 30); !got.Equal(time.Date(2026, 9, 26, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("same minute must roll over = %s", got)
	}
	if got := NextOAuthTestRun(now, 7, 0); !got.Equal(time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("earlier today must roll over = %s", got)
	}
	// A local timezone must not leak into the schedule.
	shifted := now.In(time.FixedZone("CST", 8*3600))
	if got := NextOAuthTestRun(shifted, 9, 15); !got.Equal(time.Date(2026, 9, 25, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("timezone leaked into the schedule = %s", got)
	}
}

// Every account is probed at the same time, each keeps its own card, and a
// failing account is recorded instead of ending the sweep.
func TestOAuthTestProbesEveryAccountConcurrently(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	release := make(chan struct{})
	released := false
	closeRelease := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer closeRelease()

	executor := &fakeOAuthTestExecutor{
		failOn:  "codex-b@example.com-pro.json",
		block:   release,
		barrier: 3,
		reached: make(chan struct{}),
	}
	s.SetOAuthTestExecutor(executor)

	started, err := s.StartOAuthTest(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if started != 3 {
		t.Fatalf("started = %d, want one probe per OAuth account", started)
	}
	select {
	case <-executor.reached:
	case <-time.After(3 * time.Second):
		peak := executor.peak()
		closeRelease()
		t.Fatalf("peak concurrency was %d: accounts are not probed together", peak)
	}
	closeRelease()

	cards := waitOAuthTestIdle(t, s)
	if len(cards) != 3 {
		t.Fatalf("cards = %#v", cards)
	}
	// Cards keep the provider/name order even though probes finish out of order.
	if cards[0].AuthID != "codex-a@example.com-pro.json" || cards[2].AuthID != "codex-c@example.com-pro.json" {
		t.Fatalf("card order = %#v", cards)
	}
	if cards[0].Status != "succeeded" || cards[0].Result == nil || cards[0].Result.Question2HTML == "" {
		t.Fatalf("healthy card = %#v", cards[0])
	}
	if cards[1].Status != "failed" || cards[1].Result == nil || cards[1].Result.Error == "" || cards[1].Result.CompletedAt == nil {
		t.Fatalf("failed card = %#v", cards[1])
	}
	// The API provider is not an OAuth account and gets no card.
	for _, call := range executor.recorded() {
		if strings.Contains(call.Auth.AuthID, "deepseek") {
			t.Fatalf("an API provider was probed as an OAuth account: %#v", call)
		}
	}
}

// A per-card run probes exactly one account and leaves the other cards alone.
func TestOAuthTestSingleAccountRunTouchesOneCard(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	executor := &fakeOAuthTestExecutor{}
	s.SetOAuthTestExecutor(executor)

	started, err := s.StartOAuthTest(context.Background(), "codex-b@example.com-pro.json")
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("started = %d", started)
	}
	cards := waitOAuthTestIdle(t, s)
	if len(executor.recorded()) != 1 || executor.recorded()[0].Auth.AuthID != "codex-b@example.com-pro.json" {
		t.Fatalf("calls = %#v", executor.recorded())
	}
	if cards[1].Status != "succeeded" || cards[1].Result == nil {
		t.Fatalf("card b = %#v", cards[1])
	}
	if cards[0].Status != "untested" || cards[0].Result != nil {
		t.Fatalf("card a must stay untouched = %#v", cards[0])
	}
}

// Asking for an account that is already being probed is refused, so a double
// click cannot spend the account's quota twice.
func TestOAuthTestRejectsAnAccountAlreadyRunning(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	release := make(chan struct{})
	defer close(release)
	executor := &fakeOAuthTestExecutor{block: release}
	s.SetOAuthTestExecutor(executor)

	if _, err := s.StartOAuthTest(context.Background(), "codex-a@example.com-pro.json"); err != nil {
		t.Fatal(err)
	}
	// Wait until the probe is actually in flight before asking again.
	deadline := time.Now().Add(2 * time.Second)
	for len(executor.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.StartOAuthTest(context.Background(), "codex-a@example.com-pro.json"); err == nil {
		t.Fatal("a second probe for the same account must be refused")
	}
	// A sweep skips the running account instead of failing.
	started, err := s.StartOAuthTest(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if started != 2 {
		t.Fatalf("sweep started = %d, want the two idle accounts", started)
	}
}

// Saving the schedule restarts the daily timer but must not cancel a probe in
// flight: the console saves settings while a sweep can be running.
func TestOAuthTestSettingsSaveKeepsProbesRunning(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	release := make(chan struct{})
	executor := &fakeOAuthTestExecutor{block: release}
	s.SetOAuthTestExecutor(executor)

	if _, err := s.StartOAuthTest(context.Background(), "codex-a@example.com-pro.json"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(executor.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.UpdateOAuthTestSettings(context.Background(), store.OAuthTestSettings{
		Enabled: true, HourUTC: 3, MinuteUTC: 30, Model: "gpt-6-astra", ThinkingIntensity: "high",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	defer s.StopOAuthTestScheduler()

	cards := waitOAuthTestIdle(t, s)
	if cards[0].Status != "succeeded" {
		t.Fatalf("saving settings cancelled the probe: %#v", cards[0])
	}
}

// Stopping cancels the probes that are in flight and records them as stopped.
func TestOAuthTestStopCancelsProbes(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	release := make(chan struct{})
	defer close(release)
	executor := &fakeOAuthTestExecutor{block: release}
	s.SetOAuthTestExecutor(executor)

	if _, err := s.StartOAuthTest(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(executor.recorded()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.StopOAuthTest()
	cards := waitOAuthTestIdle(t, s)
	for _, card := range cards {
		if card.Status != "failed" || card.Result == nil || !strings.Contains(card.Result.Error, "已停止") {
			t.Fatalf("stopped card = %#v", card)
		}
	}
}

// The two questions are fixed: the console compares accounts, so every account
// must be asked the same thing.
func TestOAuthTestQuestionsAreFixed(t *testing.T) {
	first, second := OAuthTestQuestions()
	if first != OAuthTestQuestion1 || second != OAuthTestQuestion2 {
		t.Fatalf("questions = %q / %q", first, second)
	}
}

// The extracted document is written next to the database, so the operator can
// open or share the exact file the card previews.
func TestOAuthTestWritesTheExtractedHTMLToAFile(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	executor := &fakeOAuthTestExecutor{}
	s.SetOAuthTestExecutor(executor)

	if _, err := s.StartOAuthTest(context.Background(), "codex-a@example.com-pro.json"); err != nil {
		t.Fatal(err)
	}
	cards := waitOAuthTestIdle(t, s)
	result := cards[0].Result
	if result == nil || result.Question2File == "" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.HasPrefix(result.Question2File, s.cfg.DataDir) {
		t.Fatalf("file %q escaped the data dir %q", result.Question2File, s.cfg.DataDir)
	}
	written, err := os.ReadFile(result.Question2File)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != result.Question2HTML {
		t.Fatalf("file content = %q, want the stored answer %q", written, result.Question2HTML)
	}
}

// Auth ids contain colons and slashes; they must never escape the data directory
// or collide with a path separator.
func TestOAuthTestFileNameCannotEscapeTheDataDir(t *testing.T) {
	for _, authID := range []string{"openai-compatibility:deepseek:1", "../../etc/passwd", "", "  ", "codex-a@example.com-pro.json"} {
		stem := oauthTestFileStem(authID)
		if stem == "" || strings.ContainsAny(stem, `/\:`) || strings.Contains(stem, "..") {
			t.Fatalf("stem for %q = %q", authID, stem)
		}
	}
}
