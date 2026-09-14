package service

import (
	"context"
	"time"
)

// FinishClientExecution releases the downstream Key concurrency slot without
// releasing auth concurrency or the financial hold. It is used when CPA has
// confirmed that the client disconnected but its nested upstream execution may
// still be alive.
func (s *Service) FinishClientExecution(ctx context.Context, reservationID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.store.FinishExecution(cleanupCtx, reservationID)
}
