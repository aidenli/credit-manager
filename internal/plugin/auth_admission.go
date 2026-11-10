package plugin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

const authAdmissionHeader = "X-Credit-Manager-Admission-Id"

func admissionResponse(response pluginapi.RequestInterceptResponse) ([]byte, error) {
	response.ClearHeaders = append(response.ClearHeaders, authAdmissionHeader, authWarmupHeader)
	return okEnvelope(response)
}

func interceptRequestBeforeAuth(raw []byte) ([]byte, error) {
	svc := service.Current()
	if svc == nil || !svc.Config().AuthOnly {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// Only the host lifecycle UUID and internal warmup nonce are consulted.
	// Bodies and metadata are never decoded.
	var req struct {
		RequestID string
		Headers   http.Header
	}
	if json.Unmarshal(raw, &req) != nil {
		return admissionResponse(quotaRejectResponse(http.StatusServiceUnavailable, service.ErrAuthAdmission.Error()))
	}
	if warmup, err := verifyWarmupAdmission(req.Headers, store.AuthIdentity{}); warmup && err == nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{ClearHeaders: []string{authAdmissionHeader}})
	}
	requestID := strings.TrimSpace(req.RequestID)
	if err := svc.RegisterAuthAdmission(requestID); err != nil {
		return admissionResponse(quotaRejectResponse(http.StatusServiceUnavailable, err.Error()))
	}
	// Clear before setting: a caller-supplied admission token never survives.
	return admissionResponse(pluginapi.RequestInterceptResponse{Headers: http.Header{authAdmissionHeader: {requestID}}})
}

// Internal warmup normally bypasses beforeAuth and already owns a warmup hold.
// OAuth diagnostic probes reuse the private nonce handshake and retain their
// existing exemption. Neither path creates a second customer admission.
func verifyWarmupAdmission(headers http.Header, auth store.AuthIdentity) (bool, error) {
	nonce := strings.TrimSpace(headers.Get(authWarmupHeader))
	if nonce == "" {
		return false, nil
	}
	activeWarmups.Lock()
	defer activeWarmups.Unlock()
	lease := activeWarmups.byNonce[nonce]
	if lease == nil || !lease.CompletedAt.IsZero() {
		return false, nil
	}
	if auth.AuthID != "" && auth.AuthID != lease.Auth.AuthID {
		return true, service.ErrAuthAdmission
	}
	if auth.Provider != "" && !warmupProvidersMatch(auth.Provider, lease.Auth.Provider) {
		return true, service.ErrAuthAdmission
	}
	return true, nil
}
