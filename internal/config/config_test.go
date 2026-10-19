package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDefaultStaleReservationTimeout(t *testing.T) {
	if got := Default().Stream.StaleReservationTimeout; got != 2*time.Hour {
		t.Fatalf("default stale timeout = %s, want 2h", got)
	}
}

func TestStaleReservationTimeoutValidation(t *testing.T) {
	for _, timeout := range []time.Duration{time.Minute, 2 * time.Hour, 24 * time.Hour} {
		cfg := Default()
		cfg.DataDir = t.TempDir()
		cfg.Stream.StaleReservationTimeout = timeout
		if err := cfg.Validate(); err != nil {
			t.Fatalf("timeout %s should be valid: %v", timeout, err)
		}
	}
	for _, timeout := range []time.Duration{0, 30 * time.Second, 25 * time.Hour} {
		cfg := Default()
		cfg.DataDir = t.TempDir()
		cfg.Stream.StaleReservationTimeout = timeout
		if err := cfg.Validate(); err == nil {
			t.Fatalf("timeout %s should be invalid", timeout)
		}
	}
}

func TestExampleConfigParses(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	cfg, err := parseYAML(raw, true)
	if err != nil {
		t.Fatalf("parse example config: %v", err)
	}
	if cfg.Stream.StaleReservationTimeout != 2*time.Hour {
		t.Fatalf("example stale timeout = %s, want 2h", cfg.Stream.StaleReservationTimeout)
	}
}

func TestAPIFallbackProviderKeysLegacy(t *testing.T) {
	cfg := Default()
	if got := cfg.APIFallback.ProviderKeys(); len(got) != 1 || got[0] != "openai-compatible-deepseek" {
		t.Fatalf("default provider keys = %v, want [openai-compatible-deepseek]", got)
	}
	cfg.APIFallback = APIFallbackConfig{Provider: "  OPENAI-COMPATIBLE-MAGPIE  "}
	if got := cfg.APIFallback.ProviderKeys(); len(got) != 1 || got[0] != "openai-compatible-magpie" {
		t.Fatalf("legacy provider keys = %v, want canonical magpie", got)
	}
	if got := (APIFallbackConfig{}).ProviderKeys(); got != nil {
		t.Fatalf("empty provider keys = %v, want nil", got)
	}
}

func TestParseYAMLAPIFallbackProviders(t *testing.T) {
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("CREDIT_MANAGER_TEST_PEPPER", "test:"+strings.Repeat("a", 32))
	raw := []byte(`
keys:
  pepper_env: CREDIT_MANAGER_TEST_PEPPER
api_fallback:
  providers:
    - " OpenAI-Compatible-Magpie "
    - openai-compatible-deepseek
  model: credit-fallback
`)
	cfg, err := ParseYAML(raw)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	want := []string{"openai-compatible-magpie", "openai-compatible-deepseek"}
	if !slices.Equal(cfg.APIFallback.Providers, want) {
		t.Fatalf("providers = %v, want %v", cfg.APIFallback.Providers, want)
	}
	if cfg.APIFallback.Model != "credit-fallback" {
		t.Fatalf("model = %q, want credit-fallback", cfg.APIFallback.Model)
	}
	if cfg.APIFallback.Provider != "openai-compatible-deepseek" {
		t.Fatalf("legacy provider changed to %q", cfg.APIFallback.Provider)
	}
	keys := cfg.APIFallback.ProviderKeys()
	if !slices.Equal(keys, want) {
		t.Fatalf("provider keys = %v, want list overriding legacy scalar", keys)
	}
	keys[0] = "changed"
	if cfg.APIFallback.Providers[0] != want[0] {
		t.Fatal("provider keys must not alias config storage")
	}
}

func TestAPIFallbackProvidersValidation(t *testing.T) {
	cases := []struct {
		name      string
		providers []string
		model     string
	}{
		{name: "empty entry", providers: []string{"openai-compatible-magpie", ""}, model: "credit-fallback"},
		{name: "whitespace entry", providers: []string{"  "}, model: "credit-fallback"},
		{name: "missing API prefix", providers: []string{"magpie"}, model: "credit-fallback"},
		{name: "empty suffix", providers: []string{"openai-compatible-"}, model: "credit-fallback"},
		{name: "space in suffix", providers: []string{"openai-compatible-bad name"}, model: "credit-fallback"},
		{name: "case-insensitive duplicate", providers: []string{"openai-compatible-magpie", " OpenAI-Compatible-MAGPIE "}, model: "credit-fallback"},
		{name: "missing model", providers: []string{"openai-compatible-magpie"}},
		{name: "whitespace model", providers: []string{"openai-compatible-magpie"}, model: "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.DataDir = t.TempDir()
			cfg.APIFallback.Providers = tc.providers
			cfg.APIFallback.Model = tc.model
			if err := cfg.Validate(); err == nil {
				t.Fatalf("providers %v model %q should be invalid", tc.providers, tc.model)
			}
		})
	}
}

func TestAPIFallbackLegacyEmptyModelStillDisables(t *testing.T) {
	cfg := Default()
	cfg.DataDir = t.TempDir()
	cfg.APIFallback = APIFallbackConfig{Provider: "openai-compatible-deepseek"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy config with empty model should validate: %v", err)
	}
}

func TestAPIFallbackProvidersExternalFileOverlay(t *testing.T) {
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("CREDIT_MANAGER_TEST_PEPPER", "test:"+strings.Repeat("b", 32))
	file := filepath.Join(t.TempDir(), "credit-manager.yaml")
	body := `
keys:
  pepper_env: CREDIT_MANAGER_TEST_PEPPER
api_fallback:
  providers:
    - openai-compatible-magpie
  model: file-model
`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write config_file: %v", err)
	}
	raw := []byte(fmt.Sprintf("config_file: %q\napi_fallback:\n  model: host-model\n", filepath.ToSlash(file)))
	cfg, err := ParseYAML(raw)
	if err != nil {
		t.Fatalf("parse overlay: %v", err)
	}
	if got := cfg.APIFallback.Providers; len(got) != 1 || got[0] != "openai-compatible-magpie" {
		t.Fatalf("providers = %v, want external allowlist", got)
	}
	if cfg.APIFallback.Model != "host-model" {
		t.Fatalf("model = %q, want host overlay", cfg.APIFallback.Model)
	}
	// An explicit empty list clears the file's allowlist and restores the
	// scalar field; merely overriding Provider must not override a nonempty list.
	raw = []byte(fmt.Sprintf("config_file: %q\napi_fallback:\n  providers: []\n  provider: openai-compatible-other\n", filepath.ToSlash(file)))
	cfg, err = ParseYAML(raw)
	if err != nil {
		t.Fatalf("parse empty-list overlay: %v", err)
	}
	if got := cfg.APIFallback.ProviderKeys(); len(got) != 1 || got[0] != "openai-compatible-other" {
		t.Fatalf("provider keys = %v, want restored scalar", got)
	}
}
