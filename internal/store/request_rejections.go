package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RequestRejection is a local failure before a reservation exists. It contains
// diagnostic attribution only; callers must supply safe reasons, not secrets,
// request/response bodies or headers.
type RequestRejection struct {
	RequestID   string
	CallerID    string
	PluginKeyID string
	Model       string
	ReasonCode  string
	Reason      string
	StatusCode  int
	CreatedAt   time.Time
}

// RecordRequestRejection records no financial effects. Duplicate request IDs are
// ignored (first write wins); blank IDs are generated and zero CreatedAt uses now.
// Strings are trimmed. Caller/key/reason code and HTTP status 400–599 are required.
// IDs/model are limited to 256 bytes, reason code to 128 and reason to 2048;
// overlong values return ErrInvalidArgument, never truncate an idempotency key.
// Caller/key IDs are attribution only and need not exist in the database.
func (s *Store) RecordRequestRejection(ctx context.Context, item RequestRejection) error {
	for _, field := range []struct {
		name  string
		value *string
		limit int
	}{
		{"request id", &item.RequestID, 256},
		{"caller id", &item.CallerID, 256},
		{"plugin key id", &item.PluginKeyID, 256},
		{"model", &item.Model, 256},
		{"reason code", &item.ReasonCode, 128},
		{"reason", &item.Reason, 2048},
	} {
		*field.value = strings.TrimSpace(*field.value)
		if len(*field.value) > field.limit {
			return fmt.Errorf("%w: rejection %s exceeds %d bytes", ErrInvalidArgument, field.name, field.limit)
		}
	}
	if item.CallerID == "" || item.PluginKeyID == "" || item.ReasonCode == "" || item.StatusCode < 400 || item.StatusCode > 599 {
		return fmt.Errorf("%w: rejection caller, plugin key, reason code and status 400–599 are required", ErrInvalidArgument)
	}
	if item.RequestID == "" {
		item.RequestID = newID()
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO request_rejections (
		request_id, caller_id, plugin_key_id, model, reason_code, reason, status_code, created_at_unix_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(request_id) DO NOTHING`,
		item.RequestID, item.CallerID, item.PluginKeyID, item.Model, item.ReasonCode, item.Reason,
		item.StatusCode, item.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("record request rejection: %w", err)
	}
	return nil
}
