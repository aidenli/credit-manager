package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestConfigureSameDatabaseSharesPendingAuthState(t *testing.T) {
	Shutdown()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	old, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(old)
	t.Cleanup(Shutdown)

	old.TrackAuthCapture("in-flight", "model-before-reconfigure")
	auth := store.AuthIdentity{Provider: "codex", AuthID: "auth-before-reconfigure"}
	if err := old.AdmitAuth(ctx, "in-flight", auth); err != nil {
		t.Fatal(err)
	}
	if err := Configure(ctx, []byte(fmt.Sprintf("data_dir: %q\n", cfg.DataDir))); err != nil {
		t.Fatal(err)
	}
	next := Current()
	if next == nil || next == old {
		t.Fatal("same-database reconfigure did not publish a new service")
	}
	next.authMu.Lock()
	active := next.activeAuthRequestsLocked(auth.Provider, auth.AuthID, "")
	next.authMu.Unlock()
	if active != 1 {
		t.Fatalf("active auth requests after reconfigure = %d, want 1", active)
	}
	next.ObserveHostUsageWithExecutor(time.Now(), auth, money.TokenUsage{Input: 7}, "codex", "model-before-reconfigure")
	usage, ok := next.CapturedHostUsage("in-flight")
	if !ok || usage.Input != 7 {
		t.Fatalf("late host usage was lost after reconfigure: %+v ok=%t", usage, ok)
	}
}

func TestConfigureRejectsDatabasePathChangeWhileRunning(t *testing.T) {
	Shutdown()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(svc)
	t.Cleanup(Shutdown)

	otherDataDir := t.TempDir()
	err = Configure(ctx, []byte(fmt.Sprintf("data_dir: %q\n", otherDataDir)))
	if err == nil {
		t.Fatal("database path change should require CPA restart")
	}
	if Current() != svc {
		t.Fatal("failed path change replaced the live service")
	}
	if _, err := svc.Store().GetCaller(ctx, BootstrapCallerID); err != nil {
		t.Fatalf("failed path change closed the live store: %v", err)
	}
}
