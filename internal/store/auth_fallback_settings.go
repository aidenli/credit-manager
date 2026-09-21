package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AuthFallbackSettings is the single-row runtime toggle for the API-provider
// fallback of bound keys. It lives in the database so the console can change it
// without editing host configuration or restarting the host.
type AuthFallbackSettings struct {
	Enabled   bool
	UpdatedAt *time.Time
}

// DefaultAuthFallbackSettings keeps the fallback off, so an upgraded deployment
// behaves exactly as before until an operator opts in. Flipping it on means a
// bound key whose own accounts are all unavailable spends money on a shared API
// provider instead of failing the request, which must stay an explicit choice.
func DefaultAuthFallbackSettings() AuthFallbackSettings {
	return AuthFallbackSettings{Enabled: false}
}

// GetAuthFallbackSettings returns the stored toggle, or the disabled default
// when the row has not been written yet.
func (s *Store) GetAuthFallbackSettings(ctx context.Context) (AuthFallbackSettings, error) {
	settings := DefaultAuthFallbackSettings()
	var enabled int
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT enabled, updated_at_unix_ms
		FROM auth_fallback_settings WHERE id = 1`).Scan(&enabled, &updated)
	if err == sql.ErrNoRows {
		return settings, nil
	}
	if err != nil {
		return AuthFallbackSettings{}, fmt.Errorf("get auth fallback settings: %w", err)
	}
	settings.Enabled = enabled != 0
	value := time.UnixMilli(updated).UTC()
	settings.UpdatedAt = &value
	return settings, nil
}

// UpsertAuthFallbackSettings stores the toggle and returns the stored row.
func (s *Store) UpsertAuthFallbackSettings(ctx context.Context, settings AuthFallbackSettings) (AuthFallbackSettings, error) {
	now := nowUnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_fallback_settings(
		id, enabled, updated_at_unix_ms
	) VALUES (1, ?, ?)
	ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled,
		updated_at_unix_ms = excluded.updated_at_unix_ms`,
		boolInt(settings.Enabled), now,
	)
	if err != nil {
		return AuthFallbackSettings{}, fmt.Errorf("save auth fallback settings: %w", err)
	}
	updated := time.UnixMilli(now).UTC()
	settings.UpdatedAt = &updated
	return settings, nil
}
