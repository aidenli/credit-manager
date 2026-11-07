package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestAuthFallbackSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	// Legacy executor replay defaults off; this switch no longer governs scheduling.
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
	// A fresh deployment reports zero hits rather than omitting the field, so the
	// console can render the counter without special-casing a missing value.
	if view["hits_total"] != float64(0) || view["hits_24h"] != float64(0) {
		t.Fatalf("default counters = %#v / %#v", view["hits_total"], view["hits_24h"])
	}
	if _, present := view["last_hit"]; present {
		t.Fatalf("default last_hit = %#v, want absent", view["last_hit"])
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

func TestAuthFallbackSettingsReportsExecutionMode(t *testing.T) {
	for _, authOnly := range []bool{false, true} {
		name := "legacy-executor"
		if authOnly {
			name = "auth-only"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("CREDIT_MANAGER_TEST_PEPPERS", "active:0123456789abcdef0123456789abcdef")
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			cfg.Keys.PepperEnv = "CREDIT_MANAGER_TEST_PEPPERS"
			cfg.Keys.ActivePepperID = "active"
			cfg.AuthOnly = authOnly
			ctx := context.Background()
			svc, err := service.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Close() })
			resp, err := getAuthFallbackSettings(ctx, svc)
			for _, method := range []string{"GET", "POST"} {
				if method == "POST" {
					resp, err = updateAuthFallbackSettings(ctx, svc, []byte(`{"enabled":true}`))
				}
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s status = %d body=%s", method, resp.StatusCode, resp.Body)
				}
				var view map[string]any
				if err := json.Unmarshal(resp.Body, &view); err != nil {
					t.Fatal(err)
				}
				if view["auth_only"] != authOnly {
					t.Fatalf("%s auth_only = %#v, want %t", method, view["auth_only"], authOnly)
				}
				if view["enabled"] != (method == "POST") {
					t.Fatalf("%s legacy toggle = %#v", method, view["enabled"])
				}
			}
			stored, err := svc.Store().GetAuthFallbackSettings(ctx)
			if err != nil || !stored.Enabled {
				t.Fatalf("legacy compatibility setting = %#v, err=%v", stored, err)
			}
		})
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

func TestAuthFallbackViewCarriesCountersAndLastHit(t *testing.T) {
	at := time.Date(2026, 9, 21, 3, 4, 5, 0, time.UTC)
	raw, err := json.Marshal(authFallbackView(service.AuthFallbackStatus{
		Settings:   store.AuthFallbackSettings{Enabled: true, UpdatedAt: &at},
		TotalHits:  7,
		RecentHits: 2,
		LastHit: &service.AuthFallbackHit{
			PluginKeyID: "key-1",
			Label:       "gd aiden",
			Provider:    "openai-compatible-agnes",
			AuthID:      "openai-compatibility:agnes:4dab06a22e06",
			Model:       "gpt-5.6-sol",
			Reason:      "not_offered",
			At:          at,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	// These are the names the console reads; a rename here breaks the counter
	// silently, so the contract is asserted as raw JSON.
	for _, want := range []string{
		`"enabled":true`,
		`"hits_total":7`,
		`"hits_24h":2`,
		`"plugin_key_id":"key-1"`,
		`"label":"gd aiden"`,
		`"provider":"openai-compatible-agnes"`,
		`"auth_id":"openai-compatibility:agnes:4dab06a22e06"`,
		`"model":"gpt-5.6-sol"`,
		`"reason":"not_offered"`,
		`"last_hit_at":"2026-09-21T03:04:05Z"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("fallback view is missing %s: %s", want, raw)
		}
	}
}

// The host only forwards management paths the plugin declares at registration.
// A route that is handled but not declared answers 404 from the host, which is
// exactly how the fallback endpoint shipped broken once.
func TestAuthQuotaFallbackRoutesAreDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, route := range Routes() {
		declared[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		"GET credit-manager/auth-quotas/fallback",
		"POST credit-manager/auth-quotas/fallback",
		"GET credit-manager/auth-quotas/session-affinity",
		"POST credit-manager/auth-quotas/session-affinity",
		"GET credit-manager/usage/released",
	} {
		if !declared[want] {
			t.Fatalf("management route %q is not declared to the host", want)
		}
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
		"执行期 API 重放（旧模式）",
		"settings.auth_only",
		"toggle.disabled = authOnly",
		"toggle.disabled = toggle.dataset.authOnly === 'true'",
		"当前 auth_only 模式不启用",
		"API 不受此开关或旧白名单限制",
		"黏性不能越过权限或优先级",
		"settings.hits_total",
		`class="auth-quota-switches"`,
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
	// The counter must render whenever the feature is on, so an operator looking
	// for it sees "兜底 0 次" instead of nothing.
	if !strings.Contains(page, "if (enabled || total > 0)") {
		t.Fatal("console hides the fallback counter while the feature is enabled")
	}
}
