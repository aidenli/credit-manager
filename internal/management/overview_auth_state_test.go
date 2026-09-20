package management

import (
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

// The console marks enabled accounts and lists them first, so the overview must
// report the host switch per ledger identity.
func TestMarkUsedAuthsDisabled(t *testing.T) {
	used := []store.UsageAuthSummary{
		{AuthID: "codex-on@example.com-pro.json", Provider: "codex"},
		{AuthID: "codex-off@example.com-pro.json", Provider: "codex"},
		// Older rows only kept a runtime index.
		{AuthIndex: "idx-off", Provider: "codex"},
		// Not on the host at all: must stay unmarked rather than "disabled".
		{AuthID: "codex-gone@example.com-pro.json", Provider: "codex"},
		// Matches by index when both are present but the id is unknown.
		{AuthID: "codex-unknown@example.com-pro.json", AuthIndex: "idx-on", Provider: "codex"},
	}
	markUsedAuthsDisabled(used, map[string]bool{
		"codex-on@example.com-pro.json":  false,
		"codex-off@example.com-pro.json": true,
		"idx-off":                        true,
		"idx-on":                         false,
	})

	got := map[string]bool{}
	for _, item := range used {
		key := strings.TrimSpace(item.AuthID)
		if key == "" {
			key = strings.TrimSpace(item.AuthIndex)
		}
		got[key] = item.Disabled
	}
	for key, want := range map[string]bool{
		"codex-on@example.com-pro.json":      false,
		"codex-off@example.com-pro.json":     true,
		"idx-off":                            true,
		"codex-gone@example.com-pro.json":    false,
		"codex-unknown@example.com-pro.json": false,
	} {
		if got[key] != want {
			t.Fatalf("%s disabled = %t, want %t (all: %#v)", key, got[key], want, got)
		}
	}
}

// An empty or nil host map must leave every row unmarked.
func TestMarkUsedAuthsDisabledWithoutHostState(t *testing.T) {
	used := []store.UsageAuthSummary{{AuthID: "codex-any@example.com-pro.json", Disabled: false}}
	markUsedAuthsDisabled(used, nil)
	if used[0].Disabled {
		t.Fatal("nil host state must not mark accounts disabled")
	}
}
