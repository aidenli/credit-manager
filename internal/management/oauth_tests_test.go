package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

// The console saves the whole schedule in one payload. Every field it edits must
// land in the store, including the enable toggle and the interval that make the
// periodic sweep possible at all.
func TestOAuthTestSettingsEndpointsPersistTheSchedule(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	response, err := updateOAuthTestSettings(ctx, svc, []byte(`{"enabled":true,"interval_minutes":15,"model":"gpt-6-astra","thinking_intensity":"medium","prompt":"自定义题1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d body=%s", response.StatusCode, response.Body)
	}
	settings, err := svc.OAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.IntervalMinutes != 15 || settings.ThinkingIntensity != "medium" || settings.Prompt != "自定义题1" {
		t.Fatalf("settings = %#v", settings)
	}

	// A partial payload keeps the untouched fields, which is what the console's
	// single save button relies on.
	if _, err := updateOAuthTestSettings(ctx, svc, []byte(`{"interval_minutes":30}`)); err != nil {
		t.Fatal(err)
	}
	settings, err = svc.OAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.IntervalMinutes != 30 || settings.Prompt != "自定义题1" {
		t.Fatalf("partial update = %#v", settings)
	}

	// The disabled intensity the console does not offer is rejected, not stored.
	rejected, err := updateOAuthTestSettings(ctx, svc, []byte(`{"thinking_intensity":"extreme"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rejected.StatusCode)
	}

	loaded, err := getOAuthTestSettings(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Enabled         bool `json:"enabled"`
		IntervalMinutes int  `json:"interval_minutes"`
		Prompt          string
	}
	if err := json.Unmarshal(loaded.Body, &view); err != nil {
		t.Fatalf("settings body is not JSON: %v (%s)", err, loaded.Body)
	}
	if !view.Enabled || view.IntervalMinutes != 30 || view.Prompt != "自定义题1" {
		t.Fatalf("loaded = %#v", view)
	}
}

// Without the plugin's executor a manual run reports a conflict, and the latest
// endpoint answers with an empty run instead of an error.
func TestOAuthTestRunWithoutExecutorIsAConflict(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	response, err := runOAuthTests(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}

	latest, err := latestOAuthTests(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if latest.StatusCode != http.StatusOK {
		t.Fatalf("latest status = %d", latest.StatusCode)
	}
	var run store.OAuthTestRun
	if err := json.Unmarshal(latest.Body, &run); err != nil {
		t.Fatalf("latest body is not JSON: %v", err)
	}
	if run.Status != "" || len(run.Results) != 0 {
		t.Fatalf("empty latest run = %#v", run)
	}
}
