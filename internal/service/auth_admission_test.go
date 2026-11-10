package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestAuthAdmissionAtomicCapLegacyHousekeepingKeepsAdmissions(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "account", 3); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{{ID: "account", Provider: "codex"}}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		id := fmt.Sprint(i)
		if err := s.RegisterAuthAdmission(id); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, handled, err := s.PickAuth(WithAuthAdmission(ctx, id, "selection"), candidates)
			if err == nil && handled {
				admitted.Add(1)
			} else if !errors.Is(err, store.ErrConcurrentLimit) {
				t.Errorf("pick handled=%v err=%v", handled, err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 3 {
		t.Fatalf("admitted=%d want 3", admitted.Load())
	}
	// Simulate all legacy housekeeping well beyond its deadline. The legacy
	// authPending/affinity sweepers must never drop strict admissions; only the
	// dedicated zombie-TTL sweep in TestAuthAdmissionZombieSweep may reclaim them.
	s.authMu.Lock()
	s.pruneAuthPendingLocked(time.Now().Add(48 * time.Hour))
	s.pruneSessionAffinityLocked(time.Now().Add(48 * time.Hour))
	active := s.activeAuthRequestsLocked("codex", "account", "")
	s.authMu.Unlock()
	if active != 3 {
		t.Fatalf("active after legacy TTL=%d", active)
	}
	s.ObserveHostUsageWithExecutor(time.Now(), store.AuthIdentity{Provider: "codex", AuthID: "account"}, money.TokenUsage{Input: 1}, "codex", "model")
	item := s.withAuthConcurrency(ctx, AuthQuotaOverviewItem{Provider: "codex", AuthID: "account"})
	if item.ActiveRequests != 3 {
		t.Fatalf("usage released active attempts: %+v", item)
	}
	for i := 0; i < 64; i++ {
		s.CompleteAuthAdmission(fmt.Sprint(i))
		s.CompleteAuthAdmission(fmt.Sprint(i))
	}
	item = s.withAuthConcurrency(ctx, AuthQuotaOverviewItem{Provider: "codex", AuthID: "account"})
	if item.ActiveRequests != 0 {
		t.Fatalf("completion leaked: %+v", item)
	}
}

func TestAuthAdmissionBoundedScalarState(t *testing.T) {
	s := quotaService(t)
	for i := 0; i < maxAuthAdmissions; i++ {
		if err := s.RegisterAuthAdmission(fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RegisterAuthAdmission("full"); !errors.Is(err, ErrAuthAdmission) {
		t.Fatalf("capacity error=%v", err)
	}
	if err := s.RegisterAuthAdmission("0"); err != nil {
		t.Fatal("duplicate registration at capacity", err)
	}
	s.CompleteAuthAdmission("0")
	if err := s.RegisterAuthAdmission("replacement"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAuthAdmission(strings.Repeat("x", 129)); !errors.Is(err, ErrAuthAdmission) {
		t.Fatalf("unbounded id=%v", err)
	}
	stateType := reflect.TypeOf(authAdmission{})
	for i := 0; i < stateType.NumField(); i++ {
		field := stateType.Field(i)
		if field.Type.Kind() == reflect.String || field.Type == reflect.TypeOf(time.Time{}) {
			continue
		}
		t.Fatalf("retained non-scalar field %s", field.Name)
	}
}

func TestAuthAdmissionSharedAcrossReconfigure(t *testing.T) {
	Shutdown()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.AuthOnly = true
	old, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(old)
	t.Cleanup(Shutdown)
	if err := old.RegisterAuthAdmission("stream"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.PickAuth(WithAuthAdmission(ctx, "stream", "one"), []AuthPickCandidate{{ID: "account", Provider: "codex"}}); err != nil {
		t.Fatal(err)
	}
	if err := Configure(ctx, []byte(fmt.Sprintf("data_dir: %q\nauth_only: true\n", cfg.DataDir))); err != nil {
		t.Fatal(err)
	}
	next := Current()
	if err := next.VerifyAuthAdmission("stream", "stream", "account", "codex"); err != nil {
		t.Fatal(err)
	}
	next.CompleteAuthAdmission("stream")
	if err := old.VerifyAuthAdmission("stream", "stream", "account", ""); !errors.Is(err, ErrAuthAdmission) {
		t.Fatalf("old state not shared: %v", err)
	}
}

func TestAuthAdmissionIncludesLegacyWarmupAndDecisionCounts(t *testing.T) {
	s, key, headers := affinityService(t, false)
	s.cfg.AuthOnly = true
	s.cfg.SessionAffinity.LogDecisions = true
	candidates := bindTwo(t, s, key.ID)
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "codex", candidates[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCaptureWithAuth("legacy", store.AuthIdentity{Provider: "codex", AuthID: candidates[0].ID}, "model")
	if err := s.RegisterAuthAdmission("new"); err != nil {
		t.Fatal(err)
	}
	id, _, decision, err := s.PickAuthForKeyWithDecision(WithAuthAdmission(context.Background(), "new", "one"), headers, candidates, "model")
	if err != nil || id != candidates[1].ID || decision == nil || decision.SelectedActiveRequests != 1 {
		t.Fatalf("id=%s decision=%+v err=%v", id, decision, err)
	}
	item := s.withAuthConcurrency(context.Background(), AuthQuotaOverviewItem{Provider: "codex", AuthID: id})
	if item.ActiveRequests != decision.SelectedActiveRequests {
		t.Fatalf("overview=%+v decision=%+v", item, decision)
	}
	s.authMu.Lock()
	s.warmupHolds["warmup"] = store.AuthIdentity{Provider: "codex", AuthID: id}
	active := s.activeAuthRequestsLocked("codex", id, "")
	s.authMu.Unlock()
	if active != 2 {
		t.Fatalf("warmup+admission=%d", active)
	}
}

func TestAuthAdmissionZombieSweepReclaimsLostCompletion(t *testing.T) {
	s := quotaService(t)
	if err := s.RegisterAuthAdmission("live"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterAuthAdmission("zombie"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.authMu.Lock()
	// Force the zombie past the TTL; the live entry keeps its fresh lastTouch.
	zombie := s.authAdmissions["zombie"]
	zombie.lastTouch = now.Add(-authAdmissionTTL - time.Minute)
	s.authAdmissions["zombie"] = zombie
	s.lastAdmissionSweep = time.Time{} // bypass the throttle for the assertion
	s.sweepAuthAdmissionsLocked(now)
	_, zombieExists := s.authAdmissions["zombie"]
	_, liveExists := s.authAdmissions["live"]
	s.authMu.Unlock()
	if zombieExists {
		t.Fatal("zombie admission survived the TTL sweep")
	}
	if !liveExists {
		t.Fatal("live admission was wrongly reclaimed")
	}
	if got := s.ActiveAuthAdmissionCount(); got != 1 {
		t.Fatalf("active after sweep=%d want 1", got)
	}
}

func TestAuthAdmissionRestartStartsEmpty(t *testing.T) {
	Shutdown()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.AuthOnly = true
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(svc)
	if err := svc.RegisterAuthAdmission("inflight"); err != nil {
		t.Fatal(err)
	}
	if got := svc.ActiveAuthAdmissionCount(); got != 1 {
		t.Fatalf("active before restart=%d", got)
	}
	// A cold restart is a brand new process: a fresh Open must start at zero even
	// though the previous instance had an in-flight admission.
	Shutdown()
	cfg2 := config.Default()
	cfg2.DataDir = t.TempDir()
	cfg2.AuthOnly = true
	svc2, err := Open(ctx, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(svc2)
	t.Cleanup(Shutdown)
	if got := svc2.ActiveAuthAdmissionCount(); got != 0 {
		t.Fatalf("restart leaked %d admissions", got)
	}
}

func TestQuiesceDrainsInFlightAdmissions(t *testing.T) {
	Shutdown()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.AuthOnly = true
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(svc)
	if err := svc.RegisterAuthAdmission("drain"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(120 * time.Millisecond)
		svc.CompleteAuthAdmission("drain")
		close(done)
	}()
	start := time.Now()
	Quiesce()
	<-done
	elapsed := time.Since(start)
	if Current() != nil {
		t.Fatal("Quiesce did not shut the service down")
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Quiesce returned before the in-flight admission drained: %v", elapsed)
	}
}
