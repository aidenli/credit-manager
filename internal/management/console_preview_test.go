package management

import (
	"os"
	"testing"
)

// TestDumpConsolePageForPreview is a development helper: it writes the assembled
// console page to CREDIT_MANAGER_PREVIEW_OUT so the toolbar layout can be
// rendered in a headless browser. It is skipped unless that variable is set.
func TestDumpConsolePageForPreview(t *testing.T) {
	out := os.Getenv("CREDIT_MANAGER_PREVIEW_OUT")
	if out == "" {
		t.Skip("CREDIT_MANAGER_PREVIEW_OUT not set")
	}
	if err := os.WriteFile(out, consolePage().Body, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", out, len(consolePage().Body))
}
