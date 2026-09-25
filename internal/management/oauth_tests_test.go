package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// The toolbar saves the whole schedule in one payload: the daily time, the
// enable toggle, the model and the intensity.
func TestOAuthTestSettingsEndpointPersistsTheDailySchedule(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	response, err := updateOAuthTestSettings(ctx, svc, []byte(`{"enabled":true,"hour_utc":21,"minute_utc":35,"model":"gpt-6-astra","thinking_intensity":"medium"}`))
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
	if !settings.Enabled || settings.HourUTC != 21 || settings.MinuteUTC != 35 || settings.ThinkingIntensity != "medium" {
		t.Fatalf("settings = %#v", settings)
	}

	// A partial payload keeps the untouched fields, which one save button relies on.
	if _, err := updateOAuthTestSettings(ctx, svc, []byte(`{"minute_utc":5}`)); err != nil {
		t.Fatal(err)
	}
	settings, err = svc.OAuthTestSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.HourUTC != 21 || settings.MinuteUTC != 5 {
		t.Fatalf("partial update = %#v", settings)
	}

	// An impossible time is rejected, not stored.
	rejected, err := updateOAuthTestSettings(ctx, svc, []byte(`{"hour_utc":25}`))
	if err != nil {
		t.Fatal(err)
	}
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rejected.StatusCode)
	}
}

// probeQuotaSource is the smallest host bridge that can list one OAuth account.
type probeQuotaSource struct{}

func (probeQuotaSource) ListAuthQuotaFiles(context.Context) ([]service.AuthQuotaFile, error) {
	return []service.AuthQuotaFile{
		{ID: "codex-a@example.com-pro.json", AuthIndex: "idx-a", Provider: "codex", Type: "codex", Email: "a@example.com"},
	}, nil
}

func (probeQuotaSource) GetAuthQuotaJSON(context.Context, string) ([]byte, error) {
	return []byte(`{"access_token":"oauth-a","email":"a@example.com"}`), nil
}

func (probeQuotaSource) DoAuthQuotaHTTP(context.Context, string, service.AuthQuotaHTTPRequest) (service.AuthQuotaHTTPResponse, error) {
	return service.AuthQuotaHTTPResponse{StatusCode: http.StatusOK}, nil
}

// The state endpoint is what the cards render: the schedule, one entry per
// account, and the questions that will be asked.
func TestOAuthTestStateEndpointListsAccountsAndQuestions(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	svc.SetAuthQuotaSource(probeQuotaSource{})

	response, err := oauthTestState(ctx, svc, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "credit-manager/oauth-tests"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}
	var view struct {
		Settings struct {
			Enabled   bool `json:"enabled"`
			HourUTC   int  `json:"hour_utc"`
			MinuteUTC int  `json:"minute_utc"`
		} `json:"settings"`
		Accounts      []service.OAuthTestAccount `json:"accounts"`
		ServerTimeUTC string                     `json:"server_time_utc"`
		Question1     string                     `json:"question1"`
		Question2     string                     `json:"question2"`
	}
	if err := json.Unmarshal(response.Body, &view); err != nil {
		t.Fatalf("state body is not JSON: %v (%s)", err, response.Body)
	}
	if view.Question1 != service.OAuthTestQuestion1 || view.Question2 != service.OAuthTestQuestion2 {
		t.Fatalf("questions = %q / %q", view.Question1, view.Question2)
	}
	if view.ServerTimeUTC == "" {
		t.Fatal("the console needs the server time to convert the daily schedule")
	}
	if view.Settings.HourUTC != store.DefaultOAuthTestHourUTC {
		t.Fatalf("default hour = %d", view.Settings.HourUTC)
	}
	if view.Accounts == nil {
		t.Fatal("accounts must be an array, not null")
	}
	if len(view.Accounts) != 1 || view.Accounts[0].Status != "untested" || view.Accounts[0].Running {
		t.Fatalf("cards = %#v", view.Accounts)
	}
}

// The question-2 document must survive the host's HTML escaping of management
// JSON, which is why it travels base64-encoded. A payload carrying the raw
// document renders as a page of source code in the console's preview.
func TestOAuthTestStateShipsTheDocumentAsBase64(t *testing.T) {
	completed := time.Now().UTC().Truncate(time.Second)
	document := "<!DOCTYPE html>\n<html><body><svg><circle r=\"1\"/></svg></body></html>"
	view := newOAuthTestAccountView(service.OAuthTestAccount{
		Provider: "codex", AuthID: "codex-a.json", DisplayName: "a", Status: "succeeded",
		Result: &store.OAuthTestResult{
			Provider: "codex", AuthID: "codex-a.json", DisplayName: "a", Status: "succeeded",
			Question1: "日本首相是谁", Question2HTML: document, Question2File: "/data/oauth-tests/a.html",
			Model: "gpt-6-astra", StartedAt: completed, CompletedAt: &completed,
		},
	})

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "question2_html") {
		t.Fatalf("the raw document must not travel as JSON: %s", encoded)
	}
	if view.Result == nil || view.Result.Question2Base64 == "" {
		t.Fatalf("view = %#v", view)
	}
	// None of the characters the host escapes may appear in the payload value.
	if strings.ContainsAny(view.Result.Question2Base64, `<>&"'`) {
		t.Fatalf("base64 payload would be corrupted by host escaping: %q", view.Result.Question2Base64)
	}
	decoded, err := base64.StdEncoding.DecodeString(view.Result.Question2Base64)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != document {
		t.Fatalf("decoded = %q, want %q", decoded, document)
	}

	// A card without a document ships no payload at all.
	empty := newOAuthTestAccountView(service.OAuthTestAccount{AuthID: "codex-b.json", Result: &store.OAuthTestResult{AuthID: "codex-b.json", Status: "failed"}})
	if empty.Result == nil || empty.Result.Question2Base64 != "" {
		t.Fatalf("empty card = %#v", empty.Result)
	}
}

// The poll after a per-card run asks only for the accounts still running, so the
// console can patch one card instead of rebuilding the grid.
func TestOAuthTestStateFilterNarrowsToRequestedAccounts(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	svc.SetAuthQuotaSource(probeQuotaSource{})

	request := pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "credit-manager/oauth-tests",
		Query:  url.Values{"auth_ids": []string{"codex-a@example.com-pro.json"}},
	}
	response, err := oauthTestState(ctx, svc, request)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Accounts []service.OAuthTestAccount `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Accounts) != 1 || view.Accounts[0].AuthID != "codex-a@example.com-pro.json" {
		t.Fatalf("filtered cards = %#v", view.Accounts)
	}

	// An unknown id yields an empty list, never the whole grid.
	request.Query = url.Values{"auth_ids": []string{"nope.json"}}
	response, err = oauthTestState(ctx, svc, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response.Body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Accounts) != 0 {
		t.Fatalf("unknown id returned %#v", view.Accounts)
	}

	// No filter means every account.
	request.Query = nil
	response, err = oauthTestState(ctx, svc, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response.Body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Accounts) != 1 {
		t.Fatalf("unfiltered cards = %#v", view.Accounts)
	}
}

// Without the plugin's executor a run reports a conflict; an unknown account is
// refused as well.
func TestOAuthTestRunWithoutExecutorIsAConflict(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	response, err := runOAuthTests(ctx, svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}

	response, err = runOAuthTests(ctx, svc, []byte(`{"auth_id":"missing.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("unknown account status = %d", response.StatusCode)
	}
}
