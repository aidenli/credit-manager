package store

import (
	"context"
	"testing"
)

func TestAuthWarmupSettingsPersistVisualConfiguration(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	defaults, err := st.GetAuthWarmupSettings(ctx)
	if err != nil || defaults.MaxParallel != 10 || defaults.StableJitterSeconds != 30 || len(defaults.Schedules) != 0 {
		t.Fatalf("defaults = %#v, %v", defaults, err)
	}
	defaults.Schedules = []AuthWarmupSchedule{{
		ID: "morning", Name: "晨间预热", Enabled: true, Timezone: "UTC", WarmupAt: "08:55",
		Auths: []AuthWarmupAuthTarget{{Provider: "claude", AuthID: "auth-1", AuthIndex: "idx-1"}}, Models: []string{"claude-haiku", "claude-sonnet"},
	}}
	saved, err := st.UpsertAuthWarmupSettings(ctx, defaults)
	if err != nil || saved.UpdatedAt == nil {
		t.Fatalf("saved = %#v, %v", saved, err)
	}
	loaded, err := st.GetAuthWarmupSettings(ctx)
	if err != nil || len(loaded.Schedules) != 1 || loaded.Schedules[0].ID != "morning" || len(loaded.Schedules[0].Models) != 2 || loaded.Schedules[0].Models[0] != "claude-haiku" || loaded.Schedules[0].WarmupAt != "08:55" {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
}

func TestAuthWarmupSettingsRejectEnabledProviderWithoutModel(t *testing.T) {
	settings := DefaultAuthWarmupSettings()
	settings.Schedules = []AuthWarmupSchedule{{ID: "bad", Enabled: true, Timezone: "UTC", WarmupAt: "08:55", Auths: []AuthWarmupAuthTarget{{AuthIndex: "idx"}}}}
	if err := ValidateAuthWarmupSettings(settings); err == nil {
		t.Fatal("enabled provider without a model should be rejected")
	}
}

func TestAuthWarmupSettingsAcceptsDirectWarmupTime(t *testing.T) {
	settings := DefaultAuthWarmupSettings()
	settings.Schedules = []AuthWarmupSchedule{{ID: "now", Enabled: true, Timezone: "UTC", WarmupAt: "03:50", Auths: []AuthWarmupAuthTarget{{AuthIndex: "idx"}}, Models: []string{"gpt-5.6-luna"}}}
	if err := ValidateAuthWarmupSettings(settings); err != nil {
		t.Fatalf("direct warmup time should be valid: %v", err)
	}
	settings = normalizeAuthWarmupSettings(settings)
	if settings.Schedules[0].WarmupAt != "03:50" {
		t.Fatalf("warmup time was changed to %s", settings.Schedules[0].WarmupAt)
	}
}

func TestAuthWarmupSettingsPersistsIndependentTasks(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	settings := DefaultAuthWarmupSettings()
	settings.Schedules = []AuthWarmupSchedule{
		{ID: "night-codex", Name: "Codex 夜间", Enabled: true, Timezone: "UTC", WarmupAt: "08:55", Auths: []AuthWarmupAuthTarget{{Provider: "codex", AuthIndex: "codex-1"}}, Models: []string{"gpt-5.6-luna"}},
		{ID: "morning-claude", Name: "Claude 晨间", Enabled: false, Timezone: "UTC", WarmupAt: "09:50", Auths: []AuthWarmupAuthTarget{{Provider: "claude", AuthIndex: "claude-1"}}, Models: []string{"claude-sonnet-4-6"}},
	}
	if _, err := st.UpsertAuthWarmupSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetAuthWarmupSettings(ctx)
	if err != nil || len(loaded.Schedules) != 2 || loaded.Schedules[0].Auths[0].AuthIndex != "codex-1" || loaded.Schedules[1].Models[0] != "claude-sonnet-4-6" {
		t.Fatalf("independent tasks = %#v, %v", loaded.Schedules, err)
	}
}

func TestAuthWarmupSettingsPersistsWeeklyFrequency(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	settings := DefaultAuthWarmupSettings()
	settings.Schedules = []AuthWarmupSchedule{{
		ID: "weekly-codex", Enabled: true, Frequency: "weekly", Weekdays: []int{1}, Timezone: "UTC", WarmupAt: "08:55",
		Auths: []AuthWarmupAuthTarget{{Provider: "codex", AuthIndex: "codex-1"}}, Models: []string{"gpt-5.6-luna"},
	}}
	if _, err := st.UpsertAuthWarmupSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetAuthWarmupSettings(ctx)
	if err != nil || len(loaded.Schedules) != 1 || loaded.Schedules[0].Frequency != AuthWarmupFrequencyWeekly || len(loaded.Schedules[0].Weekdays) != 1 || loaded.Schedules[0].Weekdays[0] != 1 {
		t.Fatalf("weekly schedule = %#v, %v", loaded.Schedules, err)
	}
}

func TestAuthWarmupSettingsPersistsMultipleWeekdays(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	settings := DefaultAuthWarmupSettings()
	settings.Schedules = []AuthWarmupSchedule{{
		ID: "weekly-codex", Enabled: true, Frequency: AuthWarmupFrequencyWeekly, Weekdays: []int{1, 3, 5}, Timezone: "UTC", WarmupAt: "08:55",
		Auths: []AuthWarmupAuthTarget{{Provider: "codex", AuthIndex: "codex-1"}}, Models: []string{"gpt-5.6-luna"},
	}}
	if _, err := st.UpsertAuthWarmupSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetAuthWarmupSettings(ctx)
	if err != nil || len(loaded.Schedules) != 1 {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	days := loaded.Schedules[0].Weekdays
	if len(days) != 3 || days[0] != 1 || days[1] != 3 || days[2] != 5 {
		t.Fatalf("weekdays = %#v", days)
	}
}

func TestAuthWarmupSettingsDefaultsMissingFrequencyToDaily(t *testing.T) {
	settings := normalizeAuthWarmupSettings(AuthWarmupSettings{Schedules: []AuthWarmupSchedule{{ID: "old", WarmupAt: "03:50"}}})
	if settings.Schedules[0].Frequency != AuthWarmupFrequencyDaily {
		t.Fatalf("missing frequency = %q", settings.Schedules[0].Frequency)
	}
}

func TestAuthWarmupSettingsPersistsOnceFrequency(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	settings := DefaultAuthWarmupSettings()
	settings.Schedules = []AuthWarmupSchedule{{
		ID: "once-codex", Enabled: true, Frequency: AuthWarmupFrequencyOnce, Timezone: "UTC", WarmupAt: "08:55", WarmupOn: "2026-09-20",
		Auths: []AuthWarmupAuthTarget{{Provider: "codex", AuthIndex: "codex-1"}}, Models: []string{"gpt-5.6-luna"},
	}}
	if _, err := st.UpsertAuthWarmupSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetAuthWarmupSettings(ctx)
	if err != nil || len(loaded.Schedules) != 1 || loaded.Schedules[0].Frequency != AuthWarmupFrequencyOnce || loaded.Schedules[0].WarmupOn != "2026-09-20" || !loaded.Schedules[0].Enabled {
		t.Fatalf("once schedule = %#v, %v", loaded.Schedules, err)
	}
}
