package management

import (
	"strings"
	"testing"
)

// The usage account picker reads the host's live accounts, not the usage ledger.
//
// The ledger is historical: it kept deleted accounts and repeated an account
// once per re-added auth_index, so the same account appeared twice with
// conflicting labels. The auth-quotas list is the live view, so the picker now
// loads it directly and drops disabled accounts.
func TestConsoleAccountPickerUsesLiveAccounts(t *testing.T) {
	page := strings.ReplaceAll(string(consolePage().Body), "\r\n", "\n")
	for _, text := range []string{
		"async function loadAuthFilterAccounts",
		"authFilterAccounts = (await loadAuthWarmupAuths()).filter(account => !account.disabled)",
		"function authFilterSource",
		"const items = authFilterSource();",
	} {
		if !strings.Contains(page, text) {
			t.Fatalf("console account picker is not wired to the live list: %q", text)
		}
	}
	// The ledger-derived list and its enable/disable tagging are gone.
	for _, gone := range []string{
		"state.usedAuths",
		"authFilterDisabled",
		"authFilterCompare",
		"used_auths_live_only",
	} {
		if strings.Contains(page, gone) {
			t.Fatalf("obsolete account-filter code still present: %q", gone)
		}
	}
}
