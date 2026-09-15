package store

import (
	"context"
	"testing"
	"time"
)

func TestAuthWarmupRunClaimsOneScheduleSlot(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	run := AuthWarmupRun{
		ID: "warmup-1", Provider: "claude", AuthID: "auth-1", AuthIndex: "idx-1",
		Model: "claude-haiku", ScheduleKey: "scheduled:2026-09-15", StartedAt: time.Now().UTC(),
	}
	claimed, err := st.StartAuthWarmupRun(ctx, run)
	if err != nil || !claimed {
		t.Fatalf("first claim = %t, %v", claimed, err)
	}
	run.ID = "warmup-2"
	claimed, err = st.StartAuthWarmupRun(ctx, run)
	if err != nil || claimed {
		t.Fatalf("duplicate claim = %t, %v", claimed, err)
	}
	if err := st.FinishAuthWarmupRun(ctx, "warmup-1", "succeeded", "claude-haiku", 2, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	runs, err := st.ListAuthWarmupRuns(ctx, "claude", "auth-1", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	got := runs[0]
	if got.Status != "succeeded" || got.InputTokens != 2 || got.OutputTokens != 1 || !got.WindowObserved || got.CompletedAt == nil {
		t.Fatalf("run = %#v", got)
	}
}
