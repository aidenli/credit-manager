package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestSessionAffinitySettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	// Default is off, and must report a usable TTL string.
	resp, err := getAuthSessionAffinitySettings(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	if view["enabled"] != false {
		t.Fatalf("default enabled = %#v", view["enabled"])
	}
	if _, ok := view["ttl"].(string); !ok {
		t.Fatalf("ttl must be a duration string, got %#v", view["ttl"])
	}

	// Enable it.
	resp, err = updateAuthSessionAffinitySettings(ctx, svc, []byte(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	if view["enabled"] != true {
		t.Fatalf("enabled after update = %#v", view["enabled"])
	}

	// The runtime must observe it without any restart.
	effective, err := svc.AuthSessionAffinitySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Enabled {
		t.Fatal("runtime cache did not pick up the new toggle")
	}

	// TTL is applied and echoed back.
	resp, err = updateAuthSessionAffinitySettings(ctx, svc, []byte(`{"ttl":"30m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	if view["ttl"] != "30m0s" && view["ttl"] != "30m" {
		t.Fatalf("ttl = %#v", view["ttl"])
	}
	// Omitting a field must leave it unchanged.
	if view["enabled"] != true {
		t.Fatalf("enabled reset by a ttl-only update: %#v", view["enabled"])
	}

	// And it survives a reload from storage.
	reloaded, err := svc.Store().GetAuthSessionAffinitySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled || reloaded.TTL != 30*time.Minute {
		t.Fatalf("stored settings = %#v", reloaded)
	}
}

func TestSessionAffinitySettingsRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"bad duration", `{"ttl":"soon"}`},
		{"ttl too small", `{"ttl":"1s"}`},
		{"ttl too large", `{"ttl":"720h"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := updateAuthSessionAffinitySettings(ctx, svc, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", resp.StatusCode, resp.Body)
			}
		})
	}

	// A rejected write must not have changed the stored value.
	stored, err := svc.Store().GetAuthSessionAffinitySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled {
		t.Fatal("a rejected update enabled the feature")
	}
}

func TestSessionAffinitySettingsStoredRowWinsOverConfig(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	// Config default is off; a stored row must override it.
	if _, err := svc.Store().UpsertAuthSessionAffinitySettings(ctx, store.SessionAffinitySettings{
		Enabled: true,
		TTL:     2 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	effective, err := svc.AuthSessionAffinitySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Enabled || effective.TTL != 2*time.Hour {
		t.Fatalf("stored row did not win: %#v", effective)
	}
}

func TestConsoleExposesSessionAffinityToggle(t *testing.T) {
	page := strings.ReplaceAll(string(consolePage().Body), "\r\n", "\n")
	for _, text := range []string{
		`id="authSessionAffinityEnabled"`,
		`id="authSessionAffinityState"`,
		"function applySessionAffinitySettings",
		"function loadSessionAffinitySettings",
		"function saveSessionAffinitySettings",
		"credit-manager/auth-quotas/session-affinity",
		"会话黏性（绑定 Key）",
	} {
		if !strings.Contains(page, text) {
			t.Fatalf("console is missing the session affinity toggle: %q", text)
		}
	}
	// It must live in the auth-quotas toolbar, not the key modal.
	idxToggle := strings.Index(page, `id="authSessionAffinityEnabled"`)
	idxQuotas := strings.Index(page, `id="tab-auth-quotas"`)
	if idxQuotas < 0 || idxToggle < idxQuotas {
		t.Fatal("session affinity toggle is not inside the auth-quotas tab")
	}
	// It is a toolbar sibling, not a member of the filter group: the filter row
	// is narrower than the switch and previously clipped its label.
	idxFilters := strings.Index(page, `class="auth-quota-toolbar-filters"`)
	idxFiltersEnd := strings.Index(page[idxFilters:], "</div>")
	if idxFilters >= 0 && idxFiltersEnd >= 0 && idxToggle < idxFilters+idxFiltersEnd {
		t.Fatal("session affinity switch must not be nested inside the filter group")
	}
}
