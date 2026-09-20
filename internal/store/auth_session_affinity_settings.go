package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SessionAffinitySettings is the single-row runtime toggle for bound-key session
// affinity. It lives in the database so the console can change it without
// editing host configuration or restarting the host.
type SessionAffinitySettings struct {
	Enabled   bool
	TTL       time.Duration
	UpdatedAt *time.Time
}

// DefaultSessionAffinitySettings keeps the feature off, matching the shipped
// config default so an upgraded deployment behaves exactly as before.
func DefaultSessionAffinitySettings() SessionAffinitySettings {
	return SessionAffinitySettings{Enabled: false, TTL: time.Hour}
}

const (
	sessionAffinityMinTTL = time.Minute
	sessionAffinityMaxTTL = 7 * 24 * time.Hour
)

func normalizeSessionAffinitySettings(settings SessionAffinitySettings) SessionAffinitySettings {
	if settings.TTL <= 0 {
		settings.TTL = time.Hour
	}
	if settings.TTL < sessionAffinityMinTTL {
		settings.TTL = sessionAffinityMinTTL
	}
	if settings.TTL > sessionAffinityMaxTTL {
		settings.TTL = sessionAffinityMaxTTL
	}
	// Whole seconds keeps storage and display stable.
	settings.TTL = settings.TTL.Truncate(time.Second)
	return settings
}

// ValidateSessionAffinitySettings rejects a TTL outside the supported range.
func ValidateSessionAffinitySettings(settings SessionAffinitySettings) error {
	if settings.TTL < sessionAffinityMinTTL || settings.TTL > sessionAffinityMaxTTL {
		return fmt.Errorf("%w: session affinity ttl must be between 1 minute and 7 days", ErrInvalidArgument)
	}
	return nil
}

// GetAuthSessionAffinitySettings returns the stored toggle, or the disabled
// default when the row has not been written yet.
func (s *Store) GetAuthSessionAffinitySettings(ctx context.Context) (SessionAffinitySettings, error) {
	settings := DefaultSessionAffinitySettings()
	var enabled int
	var ttlSeconds int64
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT enabled, ttl_seconds, updated_at_unix_ms
		FROM auth_session_affinity_settings WHERE id = 1`).Scan(&enabled, &ttlSeconds, &updated)
	if err == sql.ErrNoRows {
		return settings, nil
	}
	if err != nil {
		return SessionAffinitySettings{}, fmt.Errorf("get session affinity settings: %w", err)
	}
	settings.Enabled = enabled != 0
	settings.TTL = time.Duration(ttlSeconds) * time.Second
	settings = normalizeSessionAffinitySettings(settings)
	value := time.UnixMilli(updated).UTC()
	settings.UpdatedAt = &value
	return settings, nil
}

// UpsertAuthSessionAffinitySettings stores the toggle and returns the stored row.
// Callers passing an explicit out-of-range TTL get an error rather than a silent
// clamp; only a zero/absent TTL falls back to the default.
func (s *Store) UpsertAuthSessionAffinitySettings(ctx context.Context, settings SessionAffinitySettings) (SessionAffinitySettings, error) {
	if settings.TTL > 0 {
		if err := ValidateSessionAffinitySettings(settings); err != nil {
			return SessionAffinitySettings{}, err
		}
	}
	settings = normalizeSessionAffinitySettings(settings)
	now := nowUnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_session_affinity_settings(
		id, enabled, ttl_seconds, updated_at_unix_ms
	) VALUES (1, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled,
		ttl_seconds = excluded.ttl_seconds,
		updated_at_unix_ms = excluded.updated_at_unix_ms`,
		boolInt(settings.Enabled), int64(settings.TTL/time.Second), now,
	)
	if err != nil {
		return SessionAffinitySettings{}, fmt.Errorf("save session affinity settings: %w", err)
	}
	updated := time.UnixMilli(now).UTC()
	settings.UpdatedAt = &updated
	return settings, nil
}
