package store

import (
	"context"
	"testing"
	"time"
)

// The daily schedule must round-trip, accept the intensities the console offers,
// and refuse a time that does not exist.
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
	if defaults.HourUTC != DefaultOAuthTestHourUTC || defaults.MinuteUTC != DefaultOAuthTestMinuteUTC || defaults.Model == "" {
		t.Fatalf("defaults = %#v", defaults)
	}

	saved, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{
		Enabled:           true,
		HourUTC:           21,
		MinuteUTC:         35,
		Model:             "gpt-6-astra",
		ThinkingIntensity: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Enabled || saved.HourUTC != 21 || saved.MinuteUTC != 35 || saved.ThinkingIntensity != "medium" {
		t.Fatalf("saved = %#v", saved)
	}
	reloaded, err := st.GetOAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled || reloaded.HourUTC != 21 || reloaded.MinuteUTC != 35 || reloaded.UpdatedAt == nil {
		t.Fatalf("reloaded = %#v", reloaded)
	}

	for _, intensity := range []string{"high", "medium", "low"} {
		if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{HourUTC: 3, MinuteUTC: 0, Model: "gpt-6-astra", ThinkingIntensity: intensity}); err != nil {
			t.Fatalf("thinking_intensity %q rejected although the console offers it: %v", intensity, err)
		}
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{HourUTC: 24, MinuteUTC: 0, Model: "gpt-6-astra", ThinkingIntensity: "high"}); err == nil {
		t.Fatal("hour 24 must be rejected")
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{HourUTC: 3, MinuteUTC: 60, Model: "gpt-6-astra", ThinkingIntensity: "high"}); err == nil {
		t.Fatal("minute 60 must be rejected")
	}
	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{HourUTC: 3, MinuteUTC: 0, Model: "gpt-6-astra", ThinkingIntensity: "extreme"}); err == nil {
		t.Fatal("an unknown intensity must be rejected")
	}
}

// Results are stored per account, so a manual test of one card never erases the
// other cards, and saving the schedule never erases any card.
func TestOAuthTestResultsSurvivePerAccountAndAcrossSettings(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()

	done := time.Now().UTC()
	for _, authID := range []string{"codex-a.json", "codex-b.json"} {
		if err := st.SaveOAuthTestResult(ctx, OAuthTestResult{
			Provider:    "codex",
			AuthID:      authID,
			DisplayName: authID,
			Status:      "succeeded",
			Question1:   "答案 " + authID,
			StartedAt:   done.Add(-time.Minute),
			CompletedAt: &done,
		}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := st.GetOAuthTestResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results["codex-a.json"].Question1 != "答案 codex-a.json" {
		t.Fatalf("results = %#v", results)
	}

	// Re-testing one account replaces only that account's card.
	if err := st.SaveOAuthTestResult(ctx, OAuthTestResult{
		Provider: "codex", AuthID: "codex-b.json", DisplayName: "codex-b.json",
		Status: "failed", Error: "上游返回 HTTP 503", StartedAt: done, CompletedAt: &done,
	}); err != nil {
		t.Fatal(err)
	}
	results, err = st.GetOAuthTestResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if results["codex-a.json"].Status != "succeeded" || results["codex-b.json"].Status != "failed" || results["codex-b.json"].Error == "" {
		t.Fatalf("results after one re-test = %#v", results)
	}

	if _, err := st.UpsertOAuthTestSettings(ctx, OAuthTestSettings{Enabled: true, HourUTC: 9, MinuteUTC: 15, Model: "gpt-6-astra", ThinkingIntensity: "high"}); err != nil {
		t.Fatal(err)
	}
	results, err = st.GetOAuthTestResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("saving settings dropped results: %#v", results)
	}
	settings, err := st.GetOAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.HourUTC != 9 || settings.MinuteUTC != 15 {
		t.Fatalf("settings = %#v", settings)
	}
}

// A result without an auth id cannot be shown on a card, so it is refused.
func TestOAuthTestResultRequiresAuthID(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	if err := st.SaveOAuthTestResult(ctx, OAuthTestResult{Status: "succeeded"}); err == nil {
		t.Fatal("a result without an auth id must be rejected")
	}
}

// v1.8.18 stored a sweep as a list of per-account results. Those rows must still
// be readable: the operator already paid for those calls, and a decode failure
// would take the whole console tab down.
func TestOAuthTestResultsReadTheOlderSweepShape(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()

	done := time.Now().UTC().Truncate(time.Second)
	legacy := `{"id":"run-1","status":"succeeded","model":"gpt-6-astra","started_at":"` + done.Add(-time.Minute).Format(time.RFC3339) + `","completed_at":"` + done.Format(time.RFC3339) + `","results":[` +
		`{"provider":"codex","auth_id":"codex-a.json","display_name":"a","status":"succeeded","question1":"旧题1 A","question2_html":"<svg>A</svg>","started_at":"` + done.Format(time.RFC3339) + `"},` +
		`{"provider":"codex","auth_id":"codex-b.json","display_name":"b","status":"failed","error":"上游返回 HTTP 503","started_at":"` + done.Format(time.RFC3339) + `"}]}`
	if _, err := st.db.ExecContext(ctx, `INSERT INTO oauth_test_state(id, config_json, latest_json, updated_at_unix_ms) VALUES(1, '{}', ?, ?)`, legacy, done.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	results, err := st.GetOAuthTestResults(ctx)
	if err != nil {
		t.Fatalf("an older sweep must still decode: %v", err)
	}
	if len(results) != 2 || results["codex-a.json"].Question1 != "旧题1 A" || results["codex-b.json"].Error == "" {
		t.Fatalf("migrated results = %#v", results)
	}
	// An older sweep recorded the model once, for the whole run.
	if results["codex-a.json"].Model != "gpt-6-astra" {
		t.Fatalf("model was not carried over from the sweep: %#v", results["codex-a.json"])
	}

	// Saving one account on top of the migrated rows keeps the rest, and writes
	// the shape the console now expects.
	if err := st.SaveOAuthTestResult(ctx, OAuthTestResult{
		Provider: "codex", AuthID: "codex-c.json", DisplayName: "c", Status: "succeeded",
		Question1: "新题1 C", StartedAt: done, CompletedAt: &done,
	}); err != nil {
		t.Fatal(err)
	}
	results, err = st.GetOAuthTestResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results["codex-a.json"].Question1 != "旧题1 A" || results["codex-c.json"].Question1 != "新题1 C" {
		t.Fatalf("results after the migration = %#v", results)
	}
}

// Unreadable content reads as "no results" instead of failing the whole tab.
func TestOAuthTestResultsTolerateGarbage(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	for _, raw := range []string{`not json`, `{"results":"nonsense"}`, `{"results":[]}`, `{}`} {
		if _, err := st.db.ExecContext(ctx, `INSERT OR REPLACE INTO oauth_test_state(id, config_json, latest_json, updated_at_unix_ms) VALUES(1, '{}', ?, 0)`, raw); err != nil {
			t.Fatal(err)
		}
		results, err := st.GetOAuthTestResults(ctx)
		if err != nil {
			t.Fatalf("%s must not error: %v", raw, err)
		}
		if len(results) != 0 {
			t.Fatalf("%s decodes to %#v", raw, results)
		}
	}
}
