package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/lockfile"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestReleaseStoppedRefusesActiveWriter(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "credit-manager.db")
	s, err := store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockfile.New().Lock(ctx, store.LockPath(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	if _, err := releaseStopped(databasePath); err == nil {
		t.Fatal("recovery should refuse a database with an active writer lock")
	}
}

func TestReleaseStoppedRefusesWriterThroughSymlink(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "credit-manager.db")
	s, err := store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "credit-manager-alias.db")
	if err := os.Symlink(databasePath, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	unlock, err := lockfile.New().Lock(ctx, store.LockPath(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	if _, err := releaseStopped(alias); err == nil {
		t.Fatal("recovery should resolve symlinks before checking the writer lock")
	}
}

func TestReleaseStoppedRefusesOpenLockedWriterThroughAlias(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "credit-manager.db")
	seed, err := store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "credit-manager-alias.db")
	if err := os.Symlink(databasePath, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	running, err := store.OpenLocked(ctx, alias, store.OpenOptions{BusyTimeout: time.Second}, lockfile.New())
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	if _, err := releaseStopped(alias); err == nil {
		t.Fatal("recovery should refuse the canonical lock held by CPA through an alias")
	}
}

func TestReleaseStoppedReleasesAllHeldReservations(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "credit-manager.db")
	s, err := store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func(opened *store.Store) { _ = opened.Close() }(s)
	caller, err := s.CreateCaller(ctx, store.CallerSpec{ID: "caller", DisplayName: "Caller", QuotaMicroUSD: 100, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.CreatePluginKey(ctx, store.PluginKeySpec{
		CallerID: caller.ID, Kid: "test-kid", KeyHash: bytes.Repeat([]byte{1}, 16), PepperID: "pepper", Fingerprint: "fingerprint",
		Principal: "principal", CallerScope: "test-caller-scope", Enabled: true, QuotaMicroUSD: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	unfinished, err := s.Reserve(ctx, store.ReserveRequest{CallerID: caller.ID, PluginKeyID: key.ID, IdempotencyKey: "unfinished", Model: "model", RequestTokenEstimate: 1, AmountMicroUSD: money.MicroUSD(1)})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := s.Reserve(ctx, store.ReserveRequest{CallerID: caller.ID, PluginKeyID: key.ID, IdempotencyKey: "finished", Model: "model", RequestTokenEstimate: 1, AmountMicroUSD: money.MicroUSD(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishExecution(ctx, finished.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := releaseStopped(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReleasedCount != 2 || len(result.ReleasedReservationIDs) != 2 || result.ReleasedReservationIDs[0] != unfinished.ID || result.ReleasedReservationIDs[1] != finished.ID {
		t.Fatalf("released %+v", result)
	}
	if result.FinishedCount != 1 || len(result.FinishedReservationIDs) != 1 || result.FinishedReservationIDs[0] != finished.ID {
		t.Fatalf("finished %+v", result)
	}

	s, err = store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gotUnfinished, err := s.GetReservation(ctx, unfinished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUnfinished.Status != store.ReservationReleased {
		t.Fatalf("unfinished status = %s", gotUnfinished.Status)
	}
	gotFinished, err := s.GetReservation(ctx, finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFinished.Status != store.ReservationReleased {
		t.Fatalf("finished status = %s", gotFinished.Status)
	}
}

func TestReleaseStoppedRequiresExistingDatabase(t *testing.T) {
	_, err := releaseStopped(filepath.Join(t.TempDir(), "missing.db"))
	if err == nil {
		t.Fatal("missing database should fail")
	}
}

func TestReleaseStoppedReturnsEmptyArrays(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "credit-manager.db")
	s, err := store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := releaseStopped(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReleasedReservationIDs == nil || result.FinishedReservationIDs == nil {
		t.Fatalf("empty arrays must not marshal as null: %+v", result)
	}
}
