package plugin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuluo688/credit-manager/internal/service"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const lifecycleRequestHeader = "X-Credit-Manager-Request-Token"

var errClientDisconnectedBeforeUpstream = errors.New("client disconnected before upstream execution started")

type streamLifecycle struct {
	svc           *service.Service
	createdAt     time.Time
	reservationID string
	canceled      bool
}

var (
	streamLifecyclesMu sync.Mutex
	streamLifecycles   = map[string]*streamLifecycle{}
)

// trackStreamLifecycle starts a request-ID correlation before CPA invokes the
// plugin executor. CPA's request.complete callback supplies the same ID when
// the downstream client disconnects before the first upstream chunk.
func trackStreamLifecycle(requestID string, svc *service.Service) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || svc == nil {
		return
	}
	now := time.Now()
	streamLifecyclesMu.Lock()
	pruneStreamLifecyclesLocked(now)
	streamLifecycles[requestID] = &streamLifecycle{svc: svc, createdAt: now}
	streamLifecyclesMu.Unlock()
}

// bindStreamLifecycle associates the executor's reservation after it is made.
// A cancellation that arrived before the executor started returns true, so the
// reservation can be released without ever starting the nested host call.
func bindStreamLifecycle(requestID, reservationID string) bool {
	requestID = strings.TrimSpace(requestID)
	reservationID = strings.TrimSpace(reservationID)
	if requestID == "" || reservationID == "" {
		return false
	}
	streamLifecyclesMu.Lock()
	defer streamLifecyclesMu.Unlock()
	state := streamLifecycles[requestID]
	if state == nil {
		return false
	}
	if state.canceled {
		delete(streamLifecycles, requestID)
		return true
	}
	state.reservationID = reservationID
	return false
}

func clearStreamLifecycle(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	streamLifecyclesMu.Lock()
	delete(streamLifecycles, requestID)
	streamLifecyclesMu.Unlock()
}

func completeStreamLifecycle(req pluginapi.RequestCompletion) {
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return
	}
	streamLifecyclesMu.Lock()
	state := streamLifecycles[requestID]
	if state == nil {
		streamLifecyclesMu.Unlock()
		return
	}
	if req.Outcome != pluginapi.RequestCompletionCanceled {
		delete(streamLifecycles, requestID)
		streamLifecyclesMu.Unlock()
		return
	}
	state.canceled = true
	reservationID := state.reservationID
	svc := state.svc
	if reservationID != "" {
		delete(streamLifecycles, requestID)
	}
	streamLifecyclesMu.Unlock()
	if reservationID != "" && svc != nil {
		// Preserve auth/upstream and financial accounting. CPA has only told us
		// the client is gone; it has not yet canceled the nested model stream.
		_ = svc.FinishClientExecution(context.Background(), reservationID)
	}
}

func lifecycleIDFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	requestID := strings.TrimSpace(headers.Get(lifecycleRequestHeader))
	headers.Del(lifecycleRequestHeader)
	return requestID
}

func pruneStreamLifecyclesLocked(now time.Time) {
	const pendingLifecycleTTL = 5 * time.Minute
	for requestID, state := range streamLifecycles {
		if state.reservationID == "" && now.Sub(state.createdAt) >= pendingLifecycleTTL {
			delete(streamLifecycles, requestID)
		}
	}
}
