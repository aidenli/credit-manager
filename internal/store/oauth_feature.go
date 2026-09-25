package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultOAuthTestModel is the model the probe asks.
	DefaultOAuthTestModel = "gpt-6-astra"
	// DefaultOAuthTestThinking is the thinking intensity the probe requests.
	DefaultOAuthTestThinking = "high"
	// DefaultOAuthTestHourUTC and DefaultOAuthTestMinuteUTC are 09:00 in UTC+8,
	// the timezone this probe is used from. The console converts the operator's
	// local time into UTC, so the schedule does not drift with the server's
	// timezone.
	DefaultOAuthTestHourUTC   = 1
	DefaultOAuthTestMinuteUTC = 0
)

// oauthTestThinkingChoices are the intensities the console offers, so validation
// and UI cannot drift apart.
var oauthTestThinkingChoices = []string{"high", "medium", "low"}

// OAuthTestSettings is the daily schedule plus the request shape used for every
// account. The probe runs once a day: it is a comparison tool, not a health
// check, and hourly sweeps only spend upstream quota.
type OAuthTestSettings struct {
	Enabled           bool       `json:"enabled"`
	HourUTC           int        `json:"hour_utc"`
	MinuteUTC         int        `json:"minute_utc"`
	Model             string     `json:"model"`
	ThinkingIntensity string     `json:"thinking_intensity"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

// OAuthTestResult is one account's outcome. Question2HTML is stored exactly as
// the model returned it: the console renders it in a sandboxed iframe and the
// probe deliberately does not grade it.
type OAuthTestResult struct {
	Provider      string `json:"provider"`
	AuthID        string `json:"auth_id"`
	AuthIndex     string `json:"auth_index,omitempty"`
	DisplayName   string `json:"display_name"`
	Status        string `json:"status"`
	Question1     string `json:"question1,omitempty"`
	Question2HTML string `json:"question2_html,omitempty"`
	// Question2File is where the extracted document was written under the plugin
	// data directory, so an operator can open or share the exact file the card
	// previews. Empty when nothing HTML could be extracted or the write failed.
	Question2File string     `json:"question2_file,omitempty"`
	Error         string     `json:"error,omitempty"`
	Model         string     `json:"model,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// oauthTestState is the JSON kept in oauth_test_state.latest_json: the most
// recent result of every tested account, keyed by auth id.
type oauthTestState struct {
	Results map[string]OAuthTestResult `json:"results"`
}

// DefaultOAuthTestSettings is what a fresh installation starts with: a daily
// time that is configured but not enabled, so nothing spends upstream quota
// until the operator turns it on.
func DefaultOAuthTestSettings() OAuthTestSettings {
	return OAuthTestSettings{
		Enabled:           false,
		HourUTC:           DefaultOAuthTestHourUTC,
		MinuteUTC:         DefaultOAuthTestMinuteUTC,
		Model:             DefaultOAuthTestModel,
		ThinkingIntensity: DefaultOAuthTestThinking,
	}
}

// ValidateOAuthTestSettings rejects a schedule the runner could not honour.
func ValidateOAuthTestSettings(settings OAuthTestSettings) error {
	if settings.HourUTC < 0 || settings.HourUTC > 23 {
		return fmt.Errorf("hour_utc must be between 0 and 23")
	}
	if settings.MinuteUTC < 0 || settings.MinuteUTC > 59 {
		return fmt.Errorf("minute_utc must be between 0 and 59")
	}
	if strings.TrimSpace(settings.Model) == "" {
		return fmt.Errorf("model is required")
	}
	intensity := strings.ToLower(strings.TrimSpace(settings.ThinkingIntensity))
	for _, choice := range oauthTestThinkingChoices {
		if intensity == choice {
			return nil
		}
	}
	return fmt.Errorf("thinking_intensity must be one of %s", strings.Join(oauthTestThinkingChoices, ", "))
}

func normalizeOAuthTestSettings(settings OAuthTestSettings) OAuthTestSettings {
	if strings.TrimSpace(settings.Model) == "" {
		settings.Model = DefaultOAuthTestModel
	}
	settings.Model = strings.TrimSpace(settings.Model)
	if strings.TrimSpace(settings.ThinkingIntensity) == "" {
		settings.ThinkingIntensity = DefaultOAuthTestThinking
	}
	settings.ThinkingIntensity = strings.ToLower(strings.TrimSpace(settings.ThinkingIntensity))
	return settings
}

// GetOAuthTestSettings returns the stored schedule, or the defaults when nothing
// has been saved yet.
func (s *Store) GetOAuthTestSettings(ctx context.Context) (OAuthTestSettings, error) {
	settings := DefaultOAuthTestSettings()
	var raw string
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT config_json, updated_at_unix_ms FROM oauth_test_state WHERE id=1`).Scan(&raw, &updated)
	if err == sql.ErrNoRows {
		return settings, nil
	}
	if err != nil {
		return settings, fmt.Errorf("get oauth test settings: %w", err)
	}
	if trimmed := strings.TrimSpace(raw); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal([]byte(trimmed), &settings); err != nil {
			return DefaultOAuthTestSettings(), fmt.Errorf("decode oauth test settings: %w", err)
		}
	}
	settings = normalizeOAuthTestSettings(settings)
	if updated > 0 {
		value := time.UnixMilli(updated).UTC()
		settings.UpdatedAt = &value
	}
	return settings, nil
}

// UpsertOAuthTestSettings stores the schedule. Settings and results live in
// separate columns, so saving one never discards the other.
func (s *Store) UpsertOAuthTestSettings(ctx context.Context, settings OAuthTestSettings) (OAuthTestSettings, error) {
	settings = normalizeOAuthTestSettings(settings)
	if err := ValidateOAuthTestSettings(settings); err != nil {
		return OAuthTestSettings{}, err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return OAuthTestSettings{}, err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO oauth_test_state(id, config_json, latest_json, updated_at_unix_ms) VALUES(1, '{}', '{}', ?)`, now.UnixMilli()); err != nil {
		return OAuthTestSettings{}, fmt.Errorf("init oauth test state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_test_state SET config_json=?, updated_at_unix_ms=? WHERE id=1`, string(raw), now.UnixMilli()); err != nil {
		return OAuthTestSettings{}, fmt.Errorf("save oauth test settings: %w", err)
	}
	settings.UpdatedAt = &now
	return settings, nil
}

// GetOAuthTestResults returns the most recent result of every tested account,
// keyed by auth id.
func (s *Store) GetOAuthTestResults(ctx context.Context) (map[string]OAuthTestResult, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT latest_json FROM oauth_test_state WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return map[string]OAuthTestResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get oauth test results: %w", err)
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return map[string]OAuthTestResult{}, nil
	}
	// oauth_test_state.latest_json changed shape: v1.8.18 stored one sweep as a
	// list of per-account results, later versions store the latest result of every
	// account keyed by auth id. Both are read here, and anything unreadable reads
	// as "no results" - a bad row must never take the whole console tab down.
	var state struct {
		Results json.RawMessage `json:"results"`
		Model   string          `json:"model"`
	}
	if err := json.Unmarshal([]byte(trimmed), &state); err != nil || len(state.Results) == 0 {
		return map[string]OAuthTestResult{}, nil
	}
	byAuthID := map[string]OAuthTestResult{}
	if err := json.Unmarshal(state.Results, &byAuthID); err == nil {
		return byAuthID, nil
	}
	var sweep []OAuthTestResult
	if err := json.Unmarshal(state.Results, &sweep); err != nil {
		return map[string]OAuthTestResult{}, nil
	}
	for _, item := range sweep {
		authID := strings.TrimSpace(item.AuthID)
		if authID == "" {
			continue
		}
		if strings.TrimSpace(item.Model) == "" {
			// An older sweep recorded the model once, for the whole run.
			item.Model = strings.TrimSpace(state.Model)
		}
		byAuthID[authID] = item
	}
	return byAuthID, nil
}

// oauthTestResultsMu serializes the read-modify-write of the results map. Probes
// finish at the same time by design, and a lost update would silently erase one
// account's card. The plugin owns the database exclusively, so a process-local
// lock is enough.
var oauthTestResultsMu sync.Mutex

// SaveOAuthTestResult records one account's outcome while every other account
// keeps its last result. The probe writes after every account so the console can
// show progress during a sweep.
func (s *Store) SaveOAuthTestResult(ctx context.Context, result OAuthTestResult) error {
	authID := strings.TrimSpace(result.AuthID)
	if authID == "" {
		return fmt.Errorf("oauth test result needs an auth id")
	}
	oauthTestResultsMu.Lock()
	defer oauthTestResultsMu.Unlock()

	results, err := s.GetOAuthTestResults(ctx)
	if err != nil {
		return err
	}
	results[authID] = result
	raw, err := json.Marshal(oauthTestState{Results: results})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO oauth_test_state(id, config_json, latest_json, updated_at_unix_ms) VALUES(1, '{}', '{}', ?)`, now.UnixMilli()); err != nil {
		return fmt.Errorf("init oauth test state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_test_state SET latest_json=?, updated_at_unix_ms=? WHERE id=1`, string(raw), now.UnixMilli()); err != nil {
		return fmt.Errorf("save oauth test result: %w", err)
	}
	return nil
}
