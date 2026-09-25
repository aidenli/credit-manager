package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// fakeOAuthTestExecutor records what the sweep asked for, and can fail, block, or
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

// waitOAuthTestRun polls the stored run until the sweep settles.
func waitOAuthTestRun(t *testing.T, s *Service) store.OAuthTestRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := s.LatestOAuthTest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "running" && run.Status != "" {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("oauth test run did not settle")
	return store.OAuthTestRun{}
}

// Every account is probed at the same time, and a failing account is recorded
// rather than cutting the sweep short: the operator reads the failures later.
func TestOAuthTestSweepProbesEveryAccountConcurrently(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	// The probes are held inside the executor so that "how many ran together" is
	// observable: a sequential sweep would never release the barrier.
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

	started, err := s.StartOAuthTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != "running" || started.Model == "" {
		t.Fatalf("started run = %#v", started)
	}

	select {
	case <-executor.reached:
	case <-time.After(3 * time.Second):
		peak := executor.peak()
		closeRelease()
		t.Fatalf("peak concurrency was %d: the sweep is not probing accounts together", peak)
	}
	closeRelease()

	run := waitOAuthTestRun(t, s)
	if len(run.Results) != 3 {
		t.Fatalf("results = %#v, want one row per OAuth account", run.Results)
	}
	// Results keep the account order even though the probes finish out of order.
	if run.Results[0].AuthID != "codex-a@example.com-pro.json" || run.Results[1].AuthID != "codex-b@example.com-pro.json" || run.Results[2].AuthID != "codex-c@example.com-pro.json" {
		t.Fatalf("results lost their order: %#v", run.Results)
	}
	if run.Results[0].Status != "succeeded" || run.Results[2].Status != "succeeded" {
		t.Fatalf("healthy accounts were not recorded as succeeded: %#v", run.Results)
	}
	if run.Results[1].Status != "failed" || run.Results[1].Error == "" || run.Results[1].CompletedAt == nil {
		t.Fatalf("failing account = %#v", run.Results[1])
	}
	if run.Status != "failed" || !strings.Contains(run.Error, "1/3") {
		t.Fatalf("run summary = %q / %q", run.Status, run.Error)
	}
	// The API-key provider is not an OAuth account and is never probed.
	calls := executor.recorded()
	if len(calls) != 3 {
		t.Fatalf("executor calls = %d, want 3", len(calls))
	}
	for _, call := range calls {
		if strings.Contains(call.Auth.AuthID, "deepseek") {
			t.Fatalf("an API provider was probed as an OAuth account: %#v", call)
		}
	}
}

// Restarting the schedule must not cancel the sweep in flight: the console saves
// settings while a sweep runs, and that must not silently abandon it.
func TestOAuthTestSchedulerRestartKeepsSweepRunning(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	executor := &fakeOAuthTestExecutor{block: make(chan struct{})}
	s.SetOAuthTestExecutor(executor)

	if _, err := s.StartOAuthTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(executor.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(executor.recorded()) == 0 {
		t.Fatal("sweep never reached the executor")
	}

	if _, err := s.UpdateOAuthTestSettings(context.Background(), store.OAuthTestSettings{
		Enabled: true, IntervalMinutes: 60, Model: "gpt-6-astra", ThinkingIntensity: "high",
	}); err != nil {
		t.Fatal(err)
	}
	close(executor.block)
	defer s.StopOAuthTestScheduler()

	run := waitOAuthTestRun(t, s)
	if run.Status != "succeeded" {
		t.Fatalf("restarting the schedule cancelled the sweep: status=%q error=%q", run.Status, run.Error)
	}
}

// The custom prompt replaces the knowledge question and leaves the SVG task in
// place, because the console previews the HTML.
func TestOAuthTestQuestionsReplaceOnlyTheFirstQuestion(t *testing.T) {
	first, second := OAuthTestQuestions("  1+1=?  ")
	if first != "1+1=?" {
		t.Fatalf("custom prompt = %q", first)
	}
	if second != OAuthTestQuestion2 {
		t.Fatalf("second question changed: %q", second)
	}
	first, second = OAuthTestQuestions("   ")
	if first != OAuthTestQuestion1 || second != OAuthTestQuestion2 {
		t.Fatalf("blank prompt = %q / %q", first, second)
	}
}

// A custom prompt reaches the executor so the run records what was actually
// asked.
func TestOAuthTestCustomPromptReachesExecutor(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(oauthTestAccounts())
	executor := &fakeOAuthTestExecutor{}
	s.SetOAuthTestExecutor(executor)
	if _, err := s.UpdateOAuthTestSettings(context.Background(), store.OAuthTestSettings{
		IntervalMinutes: 60, Model: "gpt-6-astra", ThinkingIntensity: "high", Prompt: "自定义题1",
	}); err != nil {
		t.Fatal(err)
	}
	defer s.StopOAuthTestScheduler()

	if _, err := s.StartOAuthTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := waitOAuthTestRun(t, s)
	if run.Prompt != "自定义题1" {
		t.Fatalf("run prompt = %q", run.Prompt)
	}
	calls := executor.recorded()
	if len(calls) == 0 || calls[0].Prompt != "自定义题1" {
		t.Fatalf("executor saw %#v", calls)
	}
}
