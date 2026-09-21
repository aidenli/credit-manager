package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yuluo688/credit-manager/internal/money"
)

type AuditEvent struct {
	ID             int64           `json:"id"`
	CallerID       *string         `json:"caller_id"`
	PluginKeyID    *string         `json:"plugin_key_id"`
	ReservationID  *string         `json:"reservation_id"`
	EventType      string          `json:"event_type"`
	AmountMicroUSD *money.MicroUSD `json:"amount_micro_usd"`
	DetailsJSON    string          `json:"details_json"`
	CreatedAt      time.Time       `json:"created_at"`
}

type AuditFilter struct {
	CallerID    string
	PluginKeyID string
	Limit       int
}

func (s *Store) ListAuditEvents(ctx context.Context, callerID string, limit int) ([]AuditEvent, error) {
	return s.ListAuditEventsFiltered(ctx, AuditFilter{CallerID: callerID, Limit: limit})
}

func (s *Store) ListAuditEventsFiltered(ctx context.Context, filter AuditFilter) ([]AuditEvent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, caller_id, plugin_key_id, reservation_id, event_type, amount_micro_usd, details_json, created_at_unix_ms
		FROM audit_events`
	var rows *sql.Rows
	var err error
	conds := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if strings.TrimSpace(filter.CallerID) != "" {
		conds = append(conds, "caller_id = ?")
		args = append(args, filter.CallerID)
	}
	if strings.TrimSpace(filter.PluginKeyID) != "" {
		conds = append(conds, "plugin_key_id = ?")
		args = append(args, filter.PluginKeyID)
	}
	if len(conds) == 0 {
		rows, err = s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, limit)
	} else {
		args = append(args, limit)
		rows, err = s.db.QueryContext(ctx, query+` WHERE `+strings.Join(conds, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		event, errScan := scanAuditEvent(rows)
		if errScan != nil {
			return nil, errScan
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

const auditEventColumns = `id, caller_id, plugin_key_id, reservation_id, event_type,
	amount_micro_usd, details_json, created_at_unix_ms`

func scanAuditEvent(row rowScanner) (AuditEvent, error) {
	var event AuditEvent
	var caller, key, reservation sql.NullString
	var amount sql.NullInt64
	var created int64
	if err := row.Scan(&event.ID, &caller, &key, &reservation, &event.EventType, &amount, &event.DetailsJSON, &created); err != nil {
		return AuditEvent{}, err
	}
	if caller.Valid {
		value := caller.String
		event.CallerID = &value
	}
	if key.Valid {
		value := key.String
		event.PluginKeyID = &value
	}
	if reservation.Valid {
		value := reservation.String
		event.ReservationID = &value
	}
	if amount.Valid {
		value := money.MicroUSD(amount.Int64)
		event.AmountMicroUSD = &value
	}
	event.CreatedAt = fromUnixMilli(created)
	return event, nil
}

// InsertAuditEvent persists one standalone audit event. Settlement events are
// written inside the settlement transaction instead; this is for events that
// have no transaction of their own.
func (s *Store) InsertAuditEvent(ctx context.Context, callerID, pluginKeyID, eventType, details string) error {
	if strings.TrimSpace(details) == "" {
		details = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(caller_id, plugin_key_id, event_type,
		details_json, created_at_unix_ms) VALUES (?, ?, ?, ?, ?)`,
		nullableString(callerID), nullableString(pluginKeyID), eventType, details, nowUnixMilli())
	if err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

// AuditEventStatsByType counts the events of one type. sinceUnixMilli splits the
// recent count from the total, and lastUnixMilli is 0 when nothing matched.
func (s *Store) AuditEventStatsByType(ctx context.Context, eventType string, sinceUnixMilli int64) (total, recent, lastUnixMilli int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(1),
		COALESCE(SUM(CASE WHEN created_at_unix_ms >= ? THEN 1 ELSE 0 END), 0),
		COALESCE(MAX(created_at_unix_ms), 0)
		FROM audit_events WHERE event_type = ?`, sinceUnixMilli, eventType).Scan(&total, &recent, &lastUnixMilli)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("summarize audit events: %w", err)
	}
	return total, recent, lastUnixMilli, nil
}

// LatestAuditEventByType returns the newest event of one type.
func (s *Store) LatestAuditEventByType(ctx context.Context, eventType string) (AuditEvent, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+auditEventColumns+`
		FROM audit_events WHERE event_type = ? ORDER BY id DESC LIMIT 1`, eventType)
	event, err := scanAuditEvent(row)
	if err == sql.ErrNoRows {
		return AuditEvent{}, false, nil
	}
	if err != nil {
		return AuditEvent{}, false, fmt.Errorf("read latest audit event: %w", err)
	}
	return event, true, nil
}

func insertAudit(ctx context.Context, tx *sql.Tx, callerID, pluginKeyID, reservationID, eventType string, amount money.MicroUSD, details string, now int64) error {
	if details == "" {
		details = "{}"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(caller_id, plugin_key_id, reservation_id, event_type,
		amount_micro_usd, details_json, created_at_unix_ms) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullableString(callerID), nullableString(pluginKeyID), nullableString(reservationID), eventType, amount, details, now)
	if err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}
