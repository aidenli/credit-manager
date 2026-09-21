package plugin

import (
	"encoding/json"
	"strings"

	"github.com/yuluo688/credit-manager/internal/service"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Host HostModelExecute rejects image-only models. Let the native
	// /v1/images/* path run; interceptors reserve and settle instead.
	if isNativeImageProtocol(req.SourceFormat) || service.IsImageOnlyModel(req.RequestedModel) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "native_image_protocol"})
	}
	// Only handle authenticated plugin-key traffic; exclusive auth already rejected others.
	scope := metadataString(req.Metadata, service.CallerScopeMetadataKey)
	if scope == "" && bearerPresent(req.Headers) {
		// Host may not yet inject caller_scope into router metadata; still claim authenticated Bearer traffic.
		return okEnvelope(pluginapi.ModelRouteResponse{
			Handled:    true,
			TargetKind: pluginapi.ModelRouteTargetSelf,
			Reason:     "credit_manager_authenticated",
		})
	}
	if scope == "" {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "credit_manager_caller_scope",
	})
}

func isNativeImageProtocol(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai-image", "openai-video":
		return true
	default:
		return false
	}
}
