package plugin

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// The plugin has no logger of its own. Every plugin-side failure used to be
// invisible in the host log, which made the question "which layer produced the
// text this client just showed me" unanswerable. host.log is the host's own
// logging callback, so these lines land in the same file as the request's other
// log lines and carry the same request id.
//
// Everything here is best effort: observability must never fail a request.
func hostLog(level, message string, fields map[string]any, callbackID string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	level = strings.TrimSpace(level)
	if level == "" {
		level = "info"
	}
	request := map[string]any{"level": level, "message": message}
	if len(fields) > 0 {
		request["fields"] = fields
	}
	if id := strings.TrimSpace(callbackID); id != "" {
		request["host_callback_id"] = id
	}
	_, _ = callHost(pluginabi.MethodHostLog, request)
}

// hostLogRequestFailure records why one request ended without a successful
// upstream response, including the text handed back to the client.
func hostLogRequestFailure(callbackID, model, provider, stage string, err error) {
	if err == nil {
		return
	}
	fields := map[string]any{"stage": strings.TrimSpace(stage)}
	if model = strings.TrimSpace(model); model != "" {
		fields["model"] = model
	}
	if provider = strings.TrimSpace(provider); provider != "" {
		fields["provider"] = provider
	}
	hostLog("warn", "credit-manager request failed: "+errorText(err), fields, callbackID)
}

// hostLogCompatFallback records that a request body was rewritten because an
// OpenAI-compatible API provider does not accept every field the client sent.
func hostLogCompatFallback(callbackID, model, provider string, stripped []string) {
	if len(stripped) == 0 {
		return
	}
	hostLog("warn", "credit-manager fallback request rewritten: "+strings.Join(stripped, ", "), map[string]any{
		"model":    strings.TrimSpace(model),
		"provider": strings.TrimSpace(provider),
		"stripped": strings.Join(stripped, ","),
	}, callbackID)
}

// errorText bounds an error message so one log line stays readable.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return truncateText(err.Error(), maxHostLogErrorBytes)
}

func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit]) + "...(truncated)"
}

const maxHostLogErrorBytes = 600
