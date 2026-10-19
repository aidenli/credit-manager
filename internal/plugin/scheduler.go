package plugin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yuluo688/credit-manager/internal/service"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func pickAuth(raw []byte) ([]byte, error) {
	svc := service.Current()
	if svc == nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{})
	}
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	candidates := make([]service.AuthPickCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		id := firstNonEmpty(candidate.ID)
		if id == "" {
			continue
		}
		candidates = append(candidates, service.AuthPickCandidate{ID: id, Provider: candidate.Provider, Priority: candidate.Priority, Status: candidate.Status})
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
			return errorEnvelope("warmup_rejected", err.Error()), nil
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: authID, Handled: true})
	}
	authID, handled, err := svc.PickAuthForKey(context.Background(), req.Options.Headers, candidates, req.Model)
	if err != nil {
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
