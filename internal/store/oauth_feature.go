package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultOAuthTestModel is the model the OAuth intelligence probe asks.
	DefaultOAuthTestModel = "gpt-6-astra"
	// DefaultOAuthTestThinking is the thinking intensity the probe requests.
	DefaultOAuthTestThinking = "high"
	// DefaultOAuthTestIntervalMinutes is how often the probe runs when enabled.
	DefaultOAuthTestIntervalMinutes = 60
	// MinOAuthTestIntervalMinutes and MaxOAuthTestIntervalMinutes bound the
	// operator's choice. The upper bound is one day so a daily sweep is
	// expressible and anything slower is an explicit manual habit instead.
	MinOAuthTestIntervalMinutes = 1
	MaxOAuthTestIntervalMinutes = 1440
)

// oauthTestThinkingChoices are the intensities the probe may request. The
// management console offers exactly these, so validation and UI cannot drift
// apart again.
var oauthTestThinkingChoices = []string{"high", "medium", "low"}

// OAuthTestSettings is the probe schedule plus the request shape used for every
// account in a run.
type OAuthTestSettings struct {
	Enabled           bool   `json:"enabled"`
	IntervalMinutes   int    `json:"interval_minutes"`
	Model             string `json:"model"`
	ThinkingIntensity string `json:"thinking_intensity"`
	// Prompt overrides both built-in questions when it is not empty.
	Prompt    string     `json:"prompt,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// OAuthTestResult is one account's outcome inside a run. Question2HTML is stored
// as the model returned it: the console renders it in a sandboxed iframe, and
// the probe deliberately does not grade it.
type OAuthTestResult struct {
	Provider      string     `json:"provider"`
	AuthID        string     `json:"auth_id"`
	AuthIndex     string     `json:"auth_index,omitempty"`
	DisplayName   string     `json:"display_name"`
	Status        string     `json:"status"`
	Question1     string     `json:"question1,omitempty"`
	Question2HTML string     `json:"question2_html,omitempty"`
	Error         string     `json:"error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// OAuthTestRun is one sweep across the available OAuth accounts. It stops at the
// first failing account by design: a probe that fell back to another account
// would measure the fallback instead of the account under test.
type OAuthTestRun struct {
	ID                string     `json:"id"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	Model             string     `json:"model"`
	ThinkingIntensity string     `json:"thinking_intensity"`
	// Prompt is the custom first question this run used, when one was set.
	Prompt  string            `json:"prompt,omitempty"`
	Error   string            `json:"error,omitempty"`
	Results []OAuthTestResult `json:"results"`
}

// DefaultOAuthTestSettings is the schedule a fresh installation starts with:
// configured, but not enabled, so nothing spends upstream quota until the
// operator turns it on.
func DefaultOAuthTestSettings() OAuthTestSettings {
	return OAuthTestSettings{
		Enabled:           false,
		IntervalMinutes:   DefaultOAuthTestIntervalMinutes,
		Model:             DefaultOAuthTestModel,
		ThinkingIntensity: DefaultOAuthTestThinking,
	}
}

// ValidateOAuthTestSettings rejects a schedule the scheduler could not honour.
func ValidateOAuthTestSettings(settings OAuthTestSettings) error {
	if settings.IntervalMinutes < MinOAuthTestIntervalMinutes || settings.IntervalMinutes > MaxOAuthTestIntervalMinutes {
		return fmt.Errorf("interval_minutes must be between %d and %d", MinOAuthTestIntervalMinutes, MaxOAuthTestIntervalMinutes)
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

// normalizeOAuthTestSettings fills every unset field with its default so a
// partial console payload cannot store an unusable schedule.
func normalizeOAuthTestSettings(settings OAuthTestSettings) OAuthTestSettings {
	if settings.IntervalMinutes == 0 {
		settings.IntervalMinutes = DefaultOAuthTestIntervalMinutes
	}
	if strings.TrimSpace(settings.Model) == "" {
		settings.Model = DefaultOAuthTestModel
	}
	settings.Model = strings.TrimSpace(settings.Model)
	if strings.TrimSpace(settings.ThinkingIntensity) == "" {
		settings.ThinkingIntensity = DefaultOAuthTestThinking
	}
	settings.ThinkingIntensity = strings.ToLower(strings.TrimSpace(settings.ThinkingIntensity))
	settings.Prompt = strings.TrimSpace(settings.Prompt)
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
	if strings.TrimSpace(raw) != "" && strings.TrimSpace(raw) != "{}" {
		if err := json.Unmarshal([]byte(raw), &settings); err != nil {
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

// UpsertOAuthTestSettings stores the schedule. The two columns of
// oauth_test_state are written independently, so saving settings never discards
// the latest run and vice versa.
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

// SaveOAuthTestRun stores the latest run. The probe saves after every account so
// the console can show progress while a sweep is still in flight.
func (s *Store) SaveOAuthTestRun(ctx context.Context, run OAuthTestRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO oauth_test_state(id, config_json, latest_json, updated_at_unix_ms) VALUES(1, '{}', '{}', ?)`, now.UnixMilli()); err != nil {
		return fmt.Errorf("init oauth test state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_test_state SET latest_json=?, updated_at_unix_ms=? WHERE id=1`, string(raw), now.UnixMilli()); err != nil {
		return fmt.Errorf("save oauth test run: %w", err)
	}
	return nil
}

// GetLatestOAuthTestRun returns the most recent run, or a zero value when none
// has been recorded.
func (s *Store) GetLatestOAuthTestRun(ctx context.Context) (OAuthTestRun, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT latest_json FROM oauth_test_state WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows || strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "{}" {
		return OAuthTestRun{}, nil
	}
	if err != nil {
		return OAuthTestRun{}, fmt.Errorf("get oauth test run: %w", err)
	}
	var run OAuthTestRun
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return OAuthTestRun{}, fmt.Errorf("decode oauth test run: %w", err)
	}
	return run, nil
}

// MarkOAuthTestInterrupted closes a run that the previous process left running,
// because a sweep cannot survive a plugin restart.
func (s *Store) MarkOAuthTestInterrupted(ctx context.Context) error {
	run, err := s.GetLatestOAuthTestRun(ctx)
	if err != nil || run.Status != "running" {
		return err
	}
	now := time.Now().UTC()
	run.Status = "failed"
	run.Error = "服务重启导致测试中断"
	run.CompletedAt = &now
	return s.SaveOAuthTestRun(ctx, run)
}
