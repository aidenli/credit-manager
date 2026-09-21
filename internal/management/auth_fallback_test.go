package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAuthFallbackSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	// Default is off, so an upgrade never starts spending on a shared account.
	resp, err := getAuthFallbackSettings(ctx, svc)
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

	resp, err = updateAuthFallbackSettings(ctx, svc, []byte(`{"enabled":true}`))
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
	effective, err := svc.AuthFallbackSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Enabled {
		t.Fatal("runtime cache did not pick up the new toggle")
	}

	// And it survives a reload from storage.
	reloaded, err := svc.Store().GetAuthFallbackSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled {
		t.Fatalf("stored settings = %#v", reloaded)
	}

	// An empty body leaves the value unchanged rather than resetting it.
	resp, err = updateAuthFallbackSettings(ctx, svc, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	if view["enabled"] != true {
		t.Fatalf("field-missing update changed the toggle: %#v", view["enabled"])
	}
}

func TestAuthFallbackSettingsRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	resp, err := updateAuthFallbackSettings(ctx, svc, []byte(`{`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", resp.StatusCode, resp.Body)
	}

	stored, err := svc.Store().GetAuthFallbackSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled {
		t.Fatal("a rejected update enabled the fallback")
	}
}

func TestConsoleExposesAuthFallbackToggle(t *testing.T) {
	page := strings.ReplaceAll(string(consolePage().Body), "\r\n", "\n")
	for _, text := range []string{
		`id="authFallbackEnabled"`,
		`id="authFallbackState"`,
		"function applyAuthFallbackSettings",
		"function loadAuthFallbackSettings",
		"function saveAuthFallbackSettings",
		"credit-manager/auth-quotas/fallback",
		"API 兜底（绑定 Key）",
	} {
		if !strings.Contains(page, text) {
			t.Fatalf("console is missing the API fallback toggle: %q", text)
		}
	}
	// It must live in the auth-quotas toolbar, not the key modal.
	idxToggle := strings.Index(page, `id="authFallbackEnabled"`)
	idxQuotas := strings.Index(page, `id="tab-auth-quotas"`)
	if idxQuotas < 0 || idxToggle < idxQuotas {
		t.Fatal("API fallback toggle is not inside the auth-quotas tab")
	}
	// It is a toolbar sibling, not a member of the filter group.
	idxFilters := strings.Index(page, `class="auth-quota-toolbar-filters"`)
	idxFiltersEnd := strings.Index(page[idxFilters:], "</div>")
	if idxFilters >= 0 && idxFiltersEnd >= 0 && idxToggle < idxFilters+idxFiltersEnd {
		t.Fatal("API fallback switch must not be nested inside the filter group")
	}
	// The change handler has to be wired up, or the toggle would be inert.
	if !strings.Contains(page, `$('authFallbackEnabled').addEventListener('change'`) {
		t.Fatal("console does not wire the API fallback toggle")
	}
}
