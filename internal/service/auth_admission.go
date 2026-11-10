package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

const maxAuthAdmissions = 16384

// authAdmissionTTL bounds a zombie admission whose completion callback never
// arrived. The host caps a single upstream request at roughly 300s, so any entry
// left untouched for this long is certainly orphaned; real streams stay well
// below it and refresh lastTouch at every lifecycle observation.
const authAdmissionTTL = 30 * time.Minute

// authAdmissionSweepInterval throttles opportunistic zombie pruning on the hot
// path so admission registration stays O(1) per request between sweeps.
const authAdmissionSweepInterval = time.Minute

var ErrAuthAdmission = errors.New("account admission identity unavailable")

// authAdmission contains only scalar lifecycle/selection identities and a
// last-touch timestamp, never request bodies, headers, metadata or financial
// reservations. It survives until request.complete, the next logical scheduler
// attempt, or the zombie-TTL sweep that reclaims a lost completion callback.
type authAdmission struct {
	provider    string
	authID      string
	selectionID string
	lastTouch   time.Time
}

type authAdmissionContextKey struct{}
type authAdmissionContext struct{ requestID, selectionID string }

// WithAuthAdmission opts the real plugin boundary into strict lifecycle admission.
// Direct service callers retain the legacy reservation-based behavior.
func WithAuthAdmission(ctx context.Context, requestID, selectionID string) context.Context {
	return context.WithValue(ctx, authAdmissionContextKey{}, authAdmissionContext{strings.TrimSpace(requestID), strings.TrimSpace(selectionID)})
}

func (s *Service) RegisterAuthAdmission(requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 128 {
		return ErrAuthAdmission
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := time.Now()
	s.sweepAuthAdmissionsLocked(now)
	if state, ok := s.authAdmissions[requestID]; ok {
		state.lastTouch = now
		s.authAdmissions[requestID] = state
		return nil
	}
	if len(s.authAdmissions) >= maxAuthAdmissions {
		return ErrAuthAdmission
	}
	if s.authAdmissions == nil {
		s.authAdmissions = make(map[string]authAdmission)
	}
	s.authAdmissions[requestID] = authAdmission{lastTouch: now}
	return nil
}

func (s *Service) CompleteAuthAdmission(requestID string) {
	s.authMu.Lock()
	delete(s.authAdmissions, strings.TrimSpace(requestID))
	s.authMu.Unlock()
}

// ActiveAuthAdmissionCount reports the number of registered in-flight
// admissions; used to drain before a hot-reload shutdown.
func (s *Service) ActiveAuthAdmissionCount() int {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return len(s.authAdmissions)
}

// sweepAuthAdmissionsLocked drops admissions whose completion callback was lost,
// leaving lastTouch untouched past authAdmissionTTL. Throttled to one pass per
// authAdmissionSweepInterval so the request hot path stays cheap.
func (s *Service) sweepAuthAdmissionsLocked(now time.Time) {
	if len(s.authAdmissions) == 0 {
		return
	}
	if !s.lastAdmissionSweep.IsZero() && now.Sub(s.lastAdmissionSweep) < authAdmissionSweepInterval {
		return
	}
	s.lastAdmissionSweep = now
	for id, state := range s.authAdmissions {
		if state.lastTouch.IsZero() {
			state.lastTouch = now
			s.authAdmissions[id] = state
			continue
		}
		if now.Sub(state.lastTouch) > authAdmissionTTL {
			delete(s.authAdmissions, id)
		}
	}
}

// VerifyAuthAdmission verifies the exact lifecycle and, when published by the
// host, the actual selected auth. This is a logical attempt, not a socket lease.
func (s *Service) VerifyAuthAdmission(requestID, admissionID, authID, provider string) error {
	requestID, admissionID = strings.TrimSpace(requestID), strings.TrimSpace(admissionID)
	s.authMu.Lock()
	defer s.authMu.Unlock()
	state, ok := s.authAdmissions[requestID]
	if !ok || requestID == "" || admissionID != requestID || state.authID == "" {
		return ErrAuthAdmission
	}
	if authID = strings.TrimSpace(authID); authID != "" && authID != state.authID {
		return ErrAuthAdmission
	}
	if provider = authLimitProvider(provider); provider != "" && provider != state.provider {
		return ErrAuthAdmission
	}
	state.lastTouch = time.Now()
	s.authAdmissions[requestID] = state
	return nil
}

// prepareAuthAdmissionLocked releases the prior logical attempt on a new pick.
// Duplicate selection IDs return the existing choice without counting twice.
func (s *Service) prepareAuthAdmissionLocked(ctx context.Context, candidates []AuthPickCandidate) (string, bool, error) {
	admission, strict := ctx.Value(authAdmissionContextKey{}).(authAdmissionContext)
	if !strict {
		return "", false, nil
	}
	state, ok := s.authAdmissions[admission.requestID]
	if !ok || admission.requestID == "" || len(admission.selectionID) > 128 {
		return "", true, ErrAuthAdmission
	}
	if admission.selectionID != "" && admission.selectionID == state.selectionID && state.authID != "" {
		for _, candidate := range candidates {
			if strings.TrimSpace(candidate.ID) == state.authID && authLimitProvider(candidate.Provider) == state.provider && !authStatusDisabled(candidate.Status) && effectiveAuthWeight(candidate) > 0 {
				state.lastTouch = time.Now()
				s.authAdmissions[admission.requestID] = state
				return state.authID, true, nil
			}
		}
	}
	s.authAdmissions[admission.requestID] = authAdmission{selectionID: admission.selectionID, lastTouch: time.Now()}
	return "", true, nil
}

func (s *Service) bindPickedAuthLocked(ctx context.Context, chosen AuthPickCandidate) {
	if admission, strict := ctx.Value(authAdmissionContextKey{}).(authAdmissionContext); strict {
		s.authAdmissions[admission.requestID] = authAdmission{provider: authLimitProvider(chosen.Provider), authID: strings.TrimSpace(chosen.ID), selectionID: admission.selectionID, lastTouch: time.Now()}
		return
	}
	s.bindOldestUnattributedLocked(store.AuthIdentity{AuthID: strings.TrimSpace(chosen.ID), Provider: chosen.Provider})
}
