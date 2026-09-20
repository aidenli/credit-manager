package store

import (
	"context"
	"testing"
	"time"
)

// insertUsageAuthRow writes one ledger row carrying an auth identity.
func insertUsageAuthRow(t *testing.T, st *Store, id, authID, authIndex, provider, label string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	key := newTestKey(t, ctx, st, PluginKeySpec{Kid: "ledger-key-" + id, Label: "ledger-" + id})
	if _, err := st.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO usage_ledger(
		id, reservation_id, caller_id, plugin_key_id, model, input_tokens, output_tokens,
		reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
		cost_micro_usd, source, auth_provider, auth_id, auth_index, auth_label, created_at_unix_ms
	) VALUES (?, ?, ?, ?, 'm', 1, 1, 0, 0, 0, 0, 1, 'usage', ?, ?, ?, ?, ?)`,
		id, "r-"+id, key.CallerID, key.ID, provider, authID, authIndex, label, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// The ledger is historical: re-adding an account gives it a new host-assigned
// auth_index, which must not produce a second entry for the same account.
func TestListUsedAuthsCollapsesByAccountNotIndex(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	base := time.Now().UTC().Add(-time.Hour)

	insertUsageAuthRow(t, st, "a1", "codex-aaa-same@example.com-pro.json", "idx-old", "codex", "same@example.com", base)
	insertUsageAuthRow(t, st, "a2", "codex-aaa-same@example.com-pro.json", "idx-new", "codex", "same@example.com", base.Add(time.Minute))
	insertUsageAuthRow(t, st, "b1", "codex-bbb-other@example.com-pro.json", "idx-other", "codex", "other@example.com", base)

	got, err := st.ListUsedAuths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct accounts, got %d: %#v", len(got), got)
	}
	byAccount := map[string]UsageAuthSummary{}
	for _, item := range got {
		byAccount[item.AuthID] = item
	}
	readded, ok := byAccount["codex-aaa-same@example.com-pro.json"]
	if !ok {
		t.Fatalf("re-added account missing: %#v", got)
	}
	// The latest index is the one a filter should target.
	if readded.AuthIndex != "idx-new" {
		t.Fatalf("must report the most recent auth_index, got %q", readded.AuthIndex)
	}
	if _, ok := byAccount["codex-bbb-other@example.com-pro.json"]; !ok {
		t.Fatalf("unrelated account must survive: %#v", got)
	}
}

// Entries without an auth_id must still be listed (index-only historical rows).
func TestListUsedAuthsKeepsIndexOnlyRows(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	insertUsageAuthRow(t, st, "c1", "", "runtime-only-idx", "codex", "", time.Now().UTC())

	got, err := st.ListUsedAuths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AuthIndex != "runtime-only-idx" {
		t.Fatalf("index-only row = %#v", got)
	}
}
