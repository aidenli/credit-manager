package store

import (
	"context"
	"fmt"
	"strings"
)

// FinishExecution releases only the request-concurrency slot. The financial
// hold remains in place until Settle or Release finalizes the reservation.
func (s *Store) FinishExecution(ctx context.Context, reservationID string) error {
	if strings.TrimSpace(reservationID) == "" {
		return fmt.Errorf("%w: reservation id is required", ErrInvalidArgument)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE reservations
		SET execution_finished_at_unix_ms = ?
		WHERE id = ? AND status = 'held' AND execution_finished_at_unix_ms IS NULL`, nowUnixMilli(), reservationID)
	if err != nil {
		return fmt.Errorf("finish request execution: %w", err)
	}
	return nil
}
