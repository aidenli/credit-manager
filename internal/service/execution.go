package service

import (
	"context"
	"time"
)

// FinishExecution ends request and auth concurrency while preserving the
// pending capture for delayed host usage. Settlement remains independent.
func (s *Service) FinishExecution(ctx context.Context, reservationID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := s.store.FinishExecution(cleanupCtx, reservationID)
	// Auth concurrency is in-memory and must not remain occupied if SQLite is
	// briefly unavailable. A successful settlement also releases the DB slot.
	s.FinishAuthCapture(reservationID)
	return err
}
