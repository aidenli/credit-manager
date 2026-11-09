package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
)

func TestRequestRejectionPersistenceAndFinancialIsolation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{Label: "local key", MaxConcurrentRequests: 1})
	settled, err := st.Reserve(ctx, reserveRequest(key, "settled", 7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Settle(ctx, Settlement{LedgerID: "ledger", ReservationID: settled.ID, Model: "test-model",
		Usage: money.TokenUsage{Input: 10}, CostMicroUSD: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reserve(ctx, reserveRequest(key, "held", 11)); err != nil {
		t.Fatal(err)
	}
	// Compare every financial row, including timestamps and audit entries.
	financialState := func() map[string][][]any {
		t.Helper()
		state := make(map[string][][]any)
		for _, table := range []string{"callers", "plugin_keys", "reservations", "usage_ledger", "usage_statistics", "audit_events"} {
			rows, err := st.db.QueryContext(ctx, `SELECT * FROM `+table+` ORDER BY rowid`)
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				values, targets := make([]any, len(columns)), make([]any, len(columns))
				for i := range values {
					targets[i] = &values[i]
				}
				if err := rows.Scan(targets...); err != nil {
					t.Fatal(err)
				}
				state[table] = append(state[table], values)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
		}
		return state
	}
	before := financialState()
	created := time.Now().UTC().Truncate(time.Millisecond)
	item := RequestRejection{RequestID: "request", CallerID: key.CallerID, PluginKeyID: key.ID,
		Model: "test-model", ReasonCode: "concurrency_limit", Reason: "local limit reached", StatusCode: 429, CreatedAt: created}
	if err := st.RecordRequestRejection(ctx, item); err != nil {
		t.Fatal(err)
	}
	retry := item
	retry.Reason, retry.StatusCode, retry.CreatedAt = "must not overwrite", 503, created.Add(time.Hour)
	if err := st.RecordRequestRejection(ctx, retry); err != nil {
		t.Fatal(err)
	}
	rows, total, err := st.ListReleasedReservations(ctx, ReleasedReservationFilter{IncludeRejected: true})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("dedup: rows=%+v total=%d err=%v", rows, total, err)
	}
	want := ReleasedReservation{ID: "request", Kind: "rejected", RequestID: "request", StatusCode: 429,
		PluginKeyID: key.ID, KeyLabel: key.Label, Model: item.Model, Reason: "concurrency_limit: local limit reached", CreatedAt: created}
	if !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("row=%+v want=%+v", rows[0], want)
	}
	item.RequestID, item.CreatedAt = " \t", time.Time{}
	for i := 0; i < 2; i++ {
		if err := st.RecordRequestRejection(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err = st.ListReleasedReservations(ctx, ReleasedReservationFilter{IncludeRejected: true})
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("generated ids: rows=%+v total=%d err=%v", rows, total, err)
	}
	ids := make(map[string]bool)
	for _, row := range rows {
		if row.RequestID == "" || ids[row.RequestID] || row.CreatedAt.Before(created) || row.CreatedAt.After(time.Now()) {
			t.Fatalf("invalid generated id/time: %+v", row)
		}
		ids[row.RequestID] = true
	}
	if after := financialState(); !reflect.DeepEqual(before, after) {
		t.Fatalf("financial state changed: before=%+v after=%+v", before, after)
	}
	if active, err := st.CountActiveKeyReservations(ctx, key.ID); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	if _, err := st.Reserve(ctx, reserveRequest(key, "still-blocked", 1)); !errors.Is(err, ErrConcurrentLimit) {
		t.Fatalf("concurrency enforcement changed: %v", err)
	}
}

func TestRequestRejectionValidation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	valid := RequestRejection{RequestID: "id", CallerID: "caller", PluginKeyID: "attribution-only",
		Model: "model", ReasonCode: "code", Reason: "safe reason", StatusCode: 429}
	for _, tc := range []struct {
		name string
		edit func(*RequestRejection)
	}{
		{"request id", func(r *RequestRejection) { r.RequestID = strings.Repeat("i", 257) }},
		{"caller id", func(r *RequestRejection) { r.CallerID = strings.Repeat("c", 257) }},
		{"key id", func(r *RequestRejection) { r.PluginKeyID = strings.Repeat("k", 257) }},
		{"model", func(r *RequestRejection) { r.Model = strings.Repeat("m", 257) }},
		{"reason code", func(r *RequestRejection) { r.ReasonCode = strings.Repeat("c", 129) }},
		{"reason", func(r *RequestRejection) { r.Reason = strings.Repeat("r", 2049) }},
		{"unicode bytes", func(r *RequestRejection) { r.Model = strings.Repeat("模", 86) }},
		{"empty caller", func(r *RequestRejection) { r.CallerID = " " }},
		{"empty key", func(r *RequestRejection) { r.PluginKeyID = "" }},
		{"empty code", func(r *RequestRejection) { r.ReasonCode = "" }},
		{"status low", func(r *RequestRejection) { r.StatusCode = 399 }},
		{"status high", func(r *RequestRejection) { r.StatusCode = 600 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := valid
			tc.edit(&item)
			if err := st.RecordRequestRejection(ctx, item); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if _, total, err := st.ListReleasedReservations(ctx, ReleasedReservationFilter{IncludeRejected: true}); err != nil || total != 0 {
		t.Fatalf("invalid records persisted: total=%d err=%v", total, err)
	}
	valid.RequestID, valid.Model = strings.Repeat("i", 256), strings.Repeat("m", 256)
	valid.ReasonCode, valid.Reason = strings.Repeat("c", 128), strings.Repeat("r", 2048)
	if err := st.RecordRequestRejection(ctx, valid); err != nil {
		t.Fatalf("maximum lengths rejected: %v", err)
	}
}

func TestListReleasedReservationsIncludesRejected(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{Label: "primary"})
	otherKey := newTestKey(t, ctx, st, PluginKeySpec{Kid: "other-key"})
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// Release and creation times deliberately differ: ordering uses event time,
	// whereas From/To retain the existing creation-time filter contract.
	for _, row := range []struct {
		id                string
		key               PluginKey
		created, released time.Time
	}{
		{"release-old", otherKey, base.Add(-time.Hour), base.Add(-time.Minute)},
		{"shared-id", key, base, base},
		{"release-z", key, base, base},
		{"release-late", key, base.Add(-time.Hour), base.Add(time.Minute)},
	} {
		req := reserveRequest(row.key, row.id, 5)
		req.ReservationID = row.id
		if _, err := st.Reserve(ctx, req); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Release(ctx, row.id, "upstream_error: failed"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE reservations SET created_at_unix_ms=?, released_at_unix_ms=? WHERE id=?`,
			row.created.UnixMilli(), row.released.UnixMilli(), row.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []RequestRejection{
		{RequestID: "shared-id", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "test-model", CreatedAt: base},
		{RequestID: "reject-z", CallerID: key.CallerID, PluginKeyID: key.ID, Model: "test-model", CreatedAt: base},
		{RequestID: "other-caller", CallerID: "other", PluginKeyID: "missing-key", Model: "other-model", CreatedAt: base.Add(time.Hour)},
	} {
		item.ReasonCode, item.Reason, item.StatusCode = "concurrency_limit", "local limit", 429
		if err := st.RecordRequestRejection(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	all := []string{"rejected/other-caller", "released/release-late", "rejected/shared-id", "rejected/reject-z", "released/shared-id", "released/release-z", "released/release-old"}
	for _, tc := range []struct {
		name   string
		filter ReleasedReservationFilter
		want   []string
		total  int64
	}{
		{"default", ReleasedReservationFilter{}, []string{all[1], all[4], all[5], all[6]}, 4},
		{"merged", ReleasedReservationFilter{IncludeRejected: true}, all, 7},
		{"caller", ReleasedReservationFilter{IncludeRejected: true, CallerID: " caller "}, all[1:], 6},
		{"key", ReleasedReservationFilter{IncludeRejected: true, PluginKeyID: key.ID}, all[1:6], 5},
		{"model", ReleasedReservationFilter{IncludeRejected: true, Model: " other-model "}, all[:1], 1},
		{"inclusive time", ReleasedReservationFilter{IncludeRejected: true, From: &base, To: &base}, all[2:6], 4},
		{"combined", ReleasedReservationFilter{IncludeRejected: true, CallerID: key.CallerID, PluginKeyID: key.ID, Model: "test-model", From: &base, To: &base}, all[2:6], 4},
		{"no match", ReleasedReservationFilter{IncludeRejected: true, CallerID: "absent"}, []string{}, 0},
		{"negative offset", ReleasedReservationFilter{IncludeRejected: true, Limit: 2, Offset: -1}, all[:2], 7},
		{"past end", ReleasedReservationFilter{IncludeRejected: true, Offset: 7}, []string{}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := st.ListReleasedReservations(ctx, tc.filter)
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.Kind+"/"+row.ID)
				if row.Kind == "rejected" && (row.HeldMicroUSD != 0 || row.ReleasedAt != nil || row.RequestID != row.ID || row.StatusCode != 429) {
					t.Fatalf("fake financial rejection: %+v", row)
				}
				if row.Kind == "released" && (row.HeldMicroUSD != 5 || row.ReleasedAt == nil || row.RequestID != "" || row.StatusCode != 0) {
					t.Fatalf("released fields changed: %+v", row)
				}
				if row.PluginKeyID == key.ID && row.KeyLabel != key.Label {
					t.Fatalf("missing label: %+v", row)
				}
			}
			if err != nil || total != tc.total || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows=%v total=%d err=%v; want=%v total=%d", got, total, err, tc.want, tc.total)
			}
		})
	}
	// One-row pages must preserve both kinds even when their IDs and timestamps collide.
	for i, want := range all {
		rows, total, err := st.ListReleasedReservations(ctx, ReleasedReservationFilter{IncludeRejected: true, Limit: 1, Offset: i})
		if err != nil || total != 7 || len(rows) != 1 || rows[0].Kind+"/"+rows[0].ID != want {
			t.Fatalf("page %d: rows=%+v total=%d err=%v want=%s", i, rows, total, err, want)
		}
	}
}
