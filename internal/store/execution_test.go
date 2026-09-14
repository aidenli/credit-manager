package store

import (
	"context"
	"errors"
	"testing"

	"github.com/yuluo688/credit-manager/internal/money"
)

func TestFinishExecutionReleasesOnlyConcurrency(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{MaxConcurrentRequests: 1, QuotaMicroUSD: 10})
	first, err := st.Reserve(ctx, reserveRequest(key, "first", 7))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := st.FinishExecution(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Reserve(ctx, reserveRequest(key, "too-expensive", 4)); !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("finish released financial hold: %v", err)
	}
	second, err := st.Reserve(ctx, reserveRequest(key, "second", 3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reserve(ctx, reserveRequest(key, "third", 0)); !errors.Is(err, ErrConcurrentLimit) {
		t.Fatalf("duplicate finish released another request's slot: %v", err)
	}
	settlement := Settlement{ReservationID: first.ID, Model: "test-model", CostMicroUSD: 2, Usage: money.TokenUsage{Input: 2}}
	if _, err := st.Settle(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldAmountMicroUSD != second.HeldMicroUSD || got.SettledSpendMicroUSD != 2 {
		t.Fatalf("unexpected financial accounting: %+v", got)
	}
}
