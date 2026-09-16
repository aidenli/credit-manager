package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AuthWarmupRun records one synthetic request intended to start a rolling
// upstream quota window. It is deliberately separate from customer billing.
type AuthWarmupRun struct {
	ID             string     `json:"id"`
	Provider       string     `json:"provider"`
	AuthID         string     `json:"auth_id"`
	AuthIndex      string     `json:"auth_index"`
	Model          string     `json:"model"`
	ScheduleKey    string     `json:"schedule_key"`
	Status         string     `json:"status"`
	StartedAt      time.Time  `json:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	InputTokens    int64      `json:"input_tokens"`
	OutputTokens   int64      `json:"output_tokens"`
	WindowObserved bool       `json:"window_observed"`
	ErrorCode      string     `json:"error_code,omitempty"`
}

// StartAuthWarmupRun atomically claims one provider/auth occurrence. The
// false result means this due clock was already recorded.
func (s *Store) StartAuthWarmupRun(ctx context.Context, run AuthWarmupRun) (bool, error) {
	run.ID = strings.TrimSpace(run.ID)
	run.Provider = strings.TrimSpace(run.Provider)
	run.AuthID = strings.TrimSpace(run.AuthID)
	run.AuthIndex = strings.TrimSpace(run.AuthIndex)
	run.Model = strings.TrimSpace(run.Model)
	run.ScheduleKey = strings.TrimSpace(run.ScheduleKey)
	if run.ID == "" || run.Provider == "" || run.AuthID == "" || run.Model == "" || run.ScheduleKey == "" {
		return false, fmt.Errorf("%w: warmup id, provider, auth id, model, and schedule key are required", ErrInvalidArgument)
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO auth_warmup_runs(
		id, provider, auth_id, auth_index, model, schedule_key, status, started_at_unix_ms
	) VALUES (?, ?, ?, ?, ?, ?, 'running', ?)
	ON CONFLICT(provider, auth_id, schedule_key) DO NOTHING`,
		run.ID, run.Provider, run.AuthID, run.AuthIndex, run.Model, run.ScheduleKey, run.StartedAt.UTC().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("start auth warmup run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count auth warmup run: %w", err)
	}
	return affected == 1, nil
}

// FinishAuthWarmupRun writes the terminal outcome without exposing prompts,
// upstream response content, auth JSON, or credential material.
func (s *Store) FinishAuthWarmupRun(ctx context.Context, runID, status, model string, inputTokens, outputTokens int64, windowObserved bool, errorCode string) error {
	runID = strings.TrimSpace(runID)
	status = strings.TrimSpace(status)
	if runID == "" || (status != "succeeded" && status != "skipped" && status != "failed") || inputTokens < 0 || outputTokens < 0 {
		return fmt.Errorf("%w: invalid auth warmup completion", ErrInvalidArgument)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE auth_warmup_runs
		SET status = ?, model = CASE WHEN ? = '' THEN model ELSE ? END, completed_at_unix_ms = ?, input_tokens = ?, output_tokens = ?, window_observed = ?, error_code = ?
		WHERE id = ? AND status = 'running'`,
		status, strings.TrimSpace(model), strings.TrimSpace(model), nowUnixMilli(), inputTokens, outputTokens, boolInt(windowObserved), strings.TrimSpace(errorCode), runID)
	if err != nil {
		return fmt.Errorf("finish auth warmup run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count auth warmup completion: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: auth warmup run not found or already complete", ErrInvalidArgument)
	}
	return nil
}

func (s *Store) ListAuthWarmupRuns(ctx context.Context, provider, authID string, limit int) ([]AuthWarmupRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	provider, authID = strings.TrimSpace(provider), strings.TrimSpace(authID)
	query := `SELECT id, provider, auth_id, auth_index, model, schedule_key, status, started_at_unix_ms,
		completed_at_unix_ms, input_tokens, output_tokens, window_observed, error_code
		FROM auth_warmup_runs`
	args := make([]any, 0, 3)
	if provider != "" || authID != "" {
		if provider == "" || authID == "" {
			return nil, fmt.Errorf("%w: provider and auth id must be supplied together", ErrInvalidArgument)
		}
		query += ` WHERE provider = ? AND auth_id = ?`
		args = append(args, provider, authID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY started_at_unix_ms DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list auth warmup runs: %w", err)
	}
	defer rows.Close()
	result := make([]AuthWarmupRun, 0)
	for rows.Next() {
		var row AuthWarmupRun
		var completed sql.NullInt64
		var observed int
		var started int64
		if err := rows.Scan(&row.ID, &row.Provider, &row.AuthID, &row.AuthIndex, &row.Model, &row.ScheduleKey,
			&row.Status, &started, &completed, &row.InputTokens, &row.OutputTokens, &observed, &row.ErrorCode); err != nil {
			return nil, fmt.Errorf("scan auth warmup run: %w", err)
		}
		row.StartedAt = time.UnixMilli(started).UTC()
		if completed.Valid {
			value := time.UnixMilli(completed.Int64).UTC()
			row.CompletedAt = &value
		}
		row.WindowObserved = observed != 0
		result = append(result, row)
	}
	return result, rows.Err()
}
