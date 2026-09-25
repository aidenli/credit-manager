package store

import (
	"context"
	"testing"
	"time"
)

// The probe schedule must round-trip every field the console edits, accept the
// intensities the console offers, and refuse a schedule the scheduler could not
// honour.
func TestOAuthTestSettingsRoundTripAndValidation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()

	defaults, err := st.GetOAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Enabled {
		t.Fatalf("a fresh install must not enable a probe that spends upstream quota: %#v", defaults)
	}
	if defaults.IntervalMinutes != DefaultOAuthTestIntervalMinutes || defaults.Model == "" || defaults.ThinkingIntensity == "" {
		t.Fatalf("defaults = %#v", defaults)
	}

	saved, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{
		Enabled:           true,
		IntervalMinutes:   15,
		Model:             "gpt-6-astra",
		ThinkingIntensity: "medium",
		Prompt:            "  自定义题1  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Enabled || saved.IntervalMinutes != 15 || saved.ThinkingIntensity != "medium" || saved.Prompt != "自定义题1" {
		t.Fatalf("saved = %#v", saved)
	}
	reloaded, err := st.GetOAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled || reloaded.IntervalMinutes != 15 || reloaded.Prompt != "自定义题1" || reloaded.UpdatedAt == nil {
		t.Fatalf("reloaded = %#v", reloaded)
	}

	for _, intensity := range []string{"high", "medium", "low"} {
		if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{IntervalMinutes: 60, Model: "gpt-6-astra", ThinkingIntensity: intensity}); err != nil {
			t.Fatalf("thinking_intensity %q rejected although the console offers it: %v", intensity, err)
		}
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{IntervalMinutes: MaxOAuthTestIntervalMinutes + 1, Model: "gpt-6-astra", ThinkingIntensity: "high"}); err == nil {
		t.Fatal("an interval above the cap must be rejected")
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{IntervalMinutes: 60, Model: "", ThinkingIntensity: "high"}); err != nil {
		t.Fatalf("an empty model is filled with the default, not rejected: %v", err)
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{IntervalMinutes: 60, Model: "gpt-6-astra", ThinkingIntensity: "extreme"}); err == nil {
		t.Fatal("an unknown intensity must be rejected")
	}
}

// Settings and the latest run share one row. Saving either must never blank the
// other: an operator who enables the schedule must not lose the last result, and
// a sweep must not reset the schedule.
func TestOAuthTestRunAndSettingsSurviveEachOther(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()

	started := time.Now().UTC()
	run := OAuthTestRun{
		ID:                "run-1",
		Status:            "running",
		StartedAt:         started,
		Model:             "gpt-6-astra",
		ThinkingIntensity: "high",
		Results: []OAuthTestResult{{
			Provider: "codex", AuthID: "codex-a.json", DisplayName: "a@example.com",
			Status: "running", StartedAt: started,
		}},
	}
	if err := st.SaveOAuthTestRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{
		Enabled: true, IntervalMinutes: 30, Model: "gpt-6-astra", ThinkingIntensity: "high", Prompt: "题1",
	}); err != nil {
		t.Fatal(err)
	}
	latest, err := st.GetLatestOAuthTestRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != run.ID || len(latest.Results) != 1 || latest.Results[0].AuthID != "codex-a.json" {
		t.Fatalf("settings write dropped the run: %#v", latest)
	}

	if err := st.MarkOAuthTestInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	latest, err = st.GetLatestOAuthTestRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "failed" || latest.Error == "" || latest.CompletedAt == nil {
		t.Fatalf("interrupted run not closed: %#v", latest)
	}
	settings, err := st.GetOAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.IntervalMinutes != 30 || settings.Prompt != "题1" {
		t.Fatalf("run write dropped the settings: %#v", settings)
	}
}
