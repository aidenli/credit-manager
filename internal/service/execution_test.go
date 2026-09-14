package service

import (
	"context"
	"testing"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestFinishExecutionKeepsLateAuthUsage(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	key, _, err := svc.MintKey(ctx, BootstrapCallerID, "test-cancel", 10_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.BuildReservePlan(ctx, "cancel-model", []byte(`{"max_tokens":16,"input":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Reserve(ctx, key, plan, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	svc.TrackAuthCapture(reservation.ID, plan.Model)
	auth := store.AuthIdentity{Provider: "codex", AuthID: "test-auth"}
	if err := svc.AdmitAuth(ctx, reservation.ID, auth); err != nil {
		t.Fatal(err)
	}
	if err := svc.FinishExecution(ctx, reservation.ID); err != nil {
		t.Fatal(err)
	}
	svc.authMu.Lock()
	active := svc.activeAuthRequestsLocked(auth.Provider, auth.AuthID, "")
	svc.authMu.Unlock()
	if active != 0 {
		t.Fatalf("finished auth count = %d", active)
	}
	svc.ObserveHostUsage(reservation.CreatedAt, auth, money.TokenUsage{Input: 7, Output: 2}, plan.Model)
	usage, ok := svc.CapturedHostUsage(reservation.ID)
	if !ok || usage.Input != 7 {
		t.Fatal("finishing execution discarded late usage correlation")
	}
}

func TestFinishClientExecutionKeepsAuthConcurrency(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	key, _, err := svc.MintKey(ctx, BootstrapCallerID, "client-finish", 10_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.BuildReservePlan(ctx, "client-finish-model", []byte(`{"max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Reserve(ctx, key, plan, "client-finish")
	if err != nil {
		t.Fatal(err)
	}
	auth := store.AuthIdentity{Provider: "codex", AuthID: "client-finish-auth"}
	svc.TrackAuthCapture(reservation.ID, plan.Model)
	if err := svc.AdmitAuth(ctx, reservation.ID, auth); err != nil {
		t.Fatal(err)
	}
	if err := svc.FinishClientExecution(ctx, reservation.ID); err != nil {
		t.Fatal(err)
	}
	overview, err := svc.store.GetKeyUsageOverview(ctx, key.ID, reservation.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if overview.ActiveReservations != 0 {
		t.Fatalf("active key slots = %d, want 0", overview.ActiveReservations)
	}
	svc.authMu.Lock()
	active := svc.activeAuthRequestsLocked(auth.Provider, auth.AuthID, "")
	svc.authMu.Unlock()
	if active != 1 {
		t.Fatalf("auth concurrency = %d, want 1 while upstream may still run", active)
	}
}
