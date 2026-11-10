package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/yuluo688/credit-manager/internal/service"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The deployed host supports explicit rejection; the pinned SDK predates these
// fields. Keep the additive wire extension local rather than changing dependencies.
type schedulerPickResponse struct {
	AuthID       string
	Handled      bool
	Reject       bool
	RejectCode   string
	RejectReason string
}

func pickAuth(raw []byte) ([]byte, error) {
	svc := service.Current()
	if svc == nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{})
	}
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		if svc.Config().AuthOnly {
			return okEnvelope(schedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: service.ErrAuthAdmission.Error()})
		}
		return nil, err
	}
	candidates := make([]service.AuthPickCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		id := firstNonEmpty(candidate.ID)
		if id == "" {
			continue
		}
		candidates = append(candidates, service.AuthPickCandidate{
			ID:       id,
			Provider: candidate.Provider,
			Priority: candidate.Priority,
			Status:   candidate.Status,
			Weight:   service.ParseAuthWeight(candidate.Attributes["weight"]),
		})
	}
	// Remember whether this model's window offers an OpenAI-compatible API
	// provider: the host may dispatch the attempt there even when the plugin
	// selected one of the key's own accounts, and the body has to be prepared for
	// that provider in either case.
	if pickCandidatesServeAPI(req.Providers, candidates) {
		recordCompatAPITarget(req.Model)
	}
	if authID, handled, err := warmupTarget(req.Options.Headers, candidates); handled {
		if err != nil {
			if svc.Config().AuthOnly {
				return okEnvelope(schedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: "warmup_rejected: " + err.Error()})
			}
			return errorEnvelope("warmup_rejected", err.Error()), nil
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: authID, Handled: true})
	}
	ctx := service.WithSchedulerMetadata(context.Background(), req.Options.Metadata)
	if svc.Config().AuthOnly {
		ctx = service.WithAuthAdmission(ctx, http.Header(req.Options.Headers).Get(authAdmissionHeader), metadataString(req.Options.Metadata, "selection_id"))
	}
	authID, handled, decision, err := svc.PickAuthForKeyWithDecision(ctx, req.Options.Headers, candidates, req.Model)
	if decision != nil {
		// One JSON line survives host formatters that omit structured log fields.
		// This best-effort host callback runs after the scheduler lock is released.
		if payload, encodeErr := json.Marshal(decision); encodeErr == nil {
			hostLog("info", "credit-manager session-routing "+string(payload), nil, "")
		}
	}
	if err != nil {
		if svc.Config().AuthOnly {
			return okEnvelope(schedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: authPickErrorCode(err) + ": " + err.Error()})
		}
		return errorEnvelope(authPickErrorCode(err), err.Error()), nil
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  authID,
		Handled: handled,
	})
}

func authPickErrorCode(err error) string {
	if errors.Is(err, service.ErrNoBoundAuthAvailable) {
		return "bound_auth_unavailable"
	}
	return "limit_rejected"
}
