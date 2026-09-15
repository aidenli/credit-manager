package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AuthWarmupSettings is operator-managed from the authentication quota console.
// It intentionally contains no credentials, prompts, or provider responses.
type AuthWarmupSettings struct {
	MaxParallel         int                  `json:"max_parallel"`
	StableJitterSeconds int                  `json:"stable_jitter_seconds"`
	Schedules           []AuthWarmupSchedule `json:"schedules"`
	UpdatedAt           *time.Time           `json:"updated_at,omitempty"`
}

// AuthWarmupSchedule is one independently timed set of credentials and
// candidate models. Each selected auth is warmed at most once per task/day.
type AuthWarmupSchedule struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Enabled  bool                   `json:"enabled"`
	Timezone string                 `json:"timezone"`
	WarmupAt string                 `json:"warmup_at"`
	Auths    []AuthWarmupAuthTarget `json:"auths"`
	Models   []string               `json:"models"`
}

// AuthWarmupAuthTarget identifies one credential without storing any auth
// material. AuthIndex is preferred because the host keeps it stable at runtime.
type AuthWarmupAuthTarget struct {
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Label     string `json:"label,omitempty"`
}

func DefaultAuthWarmupSettings() AuthWarmupSettings {
	return AuthWarmupSettings{
		MaxParallel:         1,
		StableJitterSeconds: 180,
		Schedules:           []AuthWarmupSchedule{},
	}
}

func (s *Store) GetAuthWarmupSettings(ctx context.Context) (AuthWarmupSettings, error) {
	settings := DefaultAuthWarmupSettings()
	var schedulesJSON string
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT max_parallel, stable_jitter_seconds, schedules_json, updated_at_unix_ms
		FROM auth_warmup_settings WHERE id = 1`).Scan(
		&settings.MaxParallel, &settings.StableJitterSeconds, &schedulesJSON, &updated,
	)
	if err == sql.ErrNoRows {
		return settings, nil
	}
	if err != nil {
		return AuthWarmupSettings{}, fmt.Errorf("get auth warmup settings: %w", err)
	}
	var payload authWarmupSettingsPayload
	if err := json.Unmarshal([]byte(schedulesJSON), &payload); err != nil {
		return AuthWarmupSettings{}, fmt.Errorf("decode auth warmup settings: %w", err)
	}
	settings.Schedules = payload.Schedules
	settings = normalizeAuthWarmupSettings(settings)
	value := time.UnixMilli(updated).UTC()
	settings.UpdatedAt = &value
	return settings, nil
}

func (s *Store) UpsertAuthWarmupSettings(ctx context.Context, settings AuthWarmupSettings) (AuthWarmupSettings, error) {
	settings = normalizeAuthWarmupSettings(settings)
	if err := ValidateAuthWarmupSettings(settings); err != nil {
		return AuthWarmupSettings{}, err
	}
	schedulesJSON, err := json.Marshal(authWarmupSettingsPayload{Schedules: settings.Schedules})
	if err != nil {
		return AuthWarmupSettings{}, fmt.Errorf("encode auth warmup schedules: %w", err)
	}
	now := nowUnixMilli()
	_, err = s.db.ExecContext(ctx, `INSERT INTO auth_warmup_settings(
		id, max_parallel, stable_jitter_seconds, schedules_json, updated_at_unix_ms
	) VALUES (1, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET max_parallel = excluded.max_parallel,
		stable_jitter_seconds = excluded.stable_jitter_seconds,
		schedules_json = excluded.schedules_json,
		updated_at_unix_ms = excluded.updated_at_unix_ms`,
		settings.MaxParallel, settings.StableJitterSeconds, string(schedulesJSON), now,
	)
	if err != nil {
		return AuthWarmupSettings{}, fmt.Errorf("save auth warmup settings: %w", err)
	}
	updated := time.UnixMilli(now).UTC()
	settings.UpdatedAt = &updated
	return settings, nil
}

func ValidateAuthWarmupSettings(settings AuthWarmupSettings) error {
	if settings.MaxParallel < 1 || settings.MaxParallel > 16 {
		return fmt.Errorf("warmup max_parallel must be between 1 and 16")
	}
	if settings.StableJitterSeconds < 0 || settings.StableJitterSeconds > 900 {
		return fmt.Errorf("warmup stable_jitter_seconds must be between 0 and 900")
	}
	if len(settings.Schedules) > 32 {
		return fmt.Errorf("warmup supports at most 32 schedules")
	}
	ids := make(map[string]struct{}, len(settings.Schedules))
	for _, schedule := range settings.Schedules {
		if strings.TrimSpace(schedule.ID) == "" {
			return fmt.Errorf("warmup schedule id is required")
		}
		if _, exists := ids[schedule.ID]; exists {
			return fmt.Errorf("warmup schedule ids must be unique")
		}
		ids[schedule.ID] = struct{}{}
		if _, err := time.LoadLocation(strings.TrimSpace(schedule.Timezone)); err != nil {
			return fmt.Errorf("warmup schedule timezone is invalid: %w", err)
		}
		if _, err := time.Parse("15:04", strings.TrimSpace(schedule.WarmupAt)); err != nil {
			return fmt.Errorf("warmup schedule warmup_at must use HH:MM")
		}
		if schedule.Enabled && len(schedule.Auths) == 0 {
			return fmt.Errorf("enabled warmup schedule requires at least one auth file")
		}
		if schedule.Enabled && len(cleanAuthWarmupModels(schedule.Models)) == 0 {
			return fmt.Errorf("enabled warmup schedule requires at least one model")
		}
		for _, auth := range schedule.Auths {
			if strings.TrimSpace(auth.AuthIndex) == "" && strings.TrimSpace(auth.AuthID) == "" {
				return fmt.Errorf("warmup schedule auth target is invalid")
			}
		}
	}
	return nil
}

type authWarmupSettingsPayload struct {
	Schedules []AuthWarmupSchedule `json:"schedules"`
}

func normalizeAuthWarmupSettings(settings AuthWarmupSettings) AuthWarmupSettings {
	defaults := DefaultAuthWarmupSettings()
	if settings.MaxParallel == 0 {
		settings.MaxParallel = defaults.MaxParallel
	}
	schedules := make([]AuthWarmupSchedule, 0, len(settings.Schedules))
	for _, schedule := range settings.Schedules {
		schedules = append(schedules, normalizeAuthWarmupSchedule(schedule))
	}
	settings.Schedules = schedules
	return settings
}

func normalizeAuthWarmupSchedule(schedule AuthWarmupSchedule) AuthWarmupSchedule {
	schedule.ID = strings.TrimSpace(schedule.ID)
	if schedule.ID == "" {
		schedule.ID = NewID()
	}
	schedule.Name = strings.TrimSpace(schedule.Name)
	if schedule.Name == "" {
		schedule.Name = "预热任务"
	}
	if strings.TrimSpace(schedule.Timezone) == "" {
		schedule.Timezone = "Asia/Shanghai"
	}
	if parsed, err := time.Parse("15:04", strings.TrimSpace(schedule.WarmupAt)); err == nil {
		schedule.WarmupAt = parsed.Format("15:04")
	}
	schedule.Models = cleanAuthWarmupModels(schedule.Models)
	schedule.Auths = cleanAuthWarmupTargets(schedule.Auths)
	return schedule
}

func cleanAuthWarmupTargets(auths []AuthWarmupAuthTarget) []AuthWarmupAuthTarget {
	unique := make(map[string]struct{}, len(auths))
	result := make([]AuthWarmupAuthTarget, 0, len(auths))
	for _, auth := range auths {
		auth.Provider = strings.TrimSpace(auth.Provider)
		auth.AuthID = strings.TrimSpace(auth.AuthID)
		auth.AuthIndex = strings.TrimSpace(auth.AuthIndex)
		auth.Label = strings.TrimSpace(auth.Label)
		key := auth.AuthIndex
		if key == "" {
			key = auth.AuthID
		}
		if key == "" {
			continue
		}
		if _, exists := unique[key]; exists {
			continue
		}
		unique[key] = struct{}{}
		result = append(result, auth)
	}
	return result
}

func cleanAuthWarmupModels(models []string) []string {
	unique := make(map[string]struct{}, len(models))
	clean := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := unique[model]; exists {
			continue
		}
		unique[model] = struct{}{}
		clean = append(clean, model)
	}
	return clean
}
