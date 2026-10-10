package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
	"github.com/yuluo688/credit-manager/internal/usageparse"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type imageHold struct {
	reservation store.Reservation
	plan        service.ReservePlan
	stopHeart   func()
}

var (
	imageHoldsMu sync.Mutex
	imageHolds   = map[string]imageHold{}
)

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	// ForcedProvider restricts the nested execution to one provider. The
	// v7.2.128 SDK this plugin compiles against does not declare the field yet,
	// so it is carried here; hosts that know it honour it and older hosts simply
	// ignore the unknown JSON member.
	ForcedProvider string `json:"forced_provider,omitempty"`
}

func execute(raw []byte) ([]byte, error) {
	svc := service.Current()
	if svc == nil {
		return errorEnvelope("service_unavailable", "credit manager is not configured"), nil
	}
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	ctx := context.Background()
	key, _, err := svc.ResolveIdentity(ctx, req.Headers, req.Metadata)
	if err != nil {
		return errorEnvelope("unauthorized", err.Error()), nil
	}
	body := requestBody(req.ExecutorRequest)
	plan, err := svc.BuildReservePlan(ctx, req.Model, body)
	if err != nil {
		if errors.Is(err, store.ErrModelDisabled) {
			return errorEnvelope("model_disabled", err.Error()), nil
		}
		return errorEnvelope("reserve_rejected", err.Error()), nil
	}
	auth := executorAuthIdentity(req.ExecutorRequest)
	reservation, err := svc.ReserveWithOptions(ctx, key, plan, "", service.ReserveOptions{
		Fallback: service.IsAPIProviderAuth(auth.Provider, auth.AuthID),
	})
	if err != nil {
		if errors.Is(err, store.ErrModelNotAllowed) {
			return errorEnvelope("model_not_allowed", err.Error()), nil
		}
		return errorEnvelope("limit_rejected", err.Error()), nil
	}
	svc.TrackAuthCaptureWithAuth(reservation.ID, auth, plan.Model, req.Model)
	defer func() { _ = svc.FinishExecution(ctx, reservation.ID) }()
	if err := admitExecutorAuth(ctx, svc, reservation.ID, auth); err != nil {
		_ = svc.Release(ctx, reservation.ID, "auth_concurrency:"+err.Error())
		return errorEnvelope("limit_rejected", err.Error()), nil
	}
	stopHeartbeat := startReservationHeartbeat(svc, reservation.ID)
	defer stopHeartbeat()

	startedAt := time.Now()
	body = prepareCompatBody(req, body)
	hostBody, headers, status, errHost := hostModelExecute(req.HostCallbackID, req.ExecutorRequest, body, false)
	completedAt := time.Now()
	metrics := usageMetricsFromRequest(body, startedAt, completedAt, resultFromStatus(status))
	if errHost != nil {
		hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "host_execute", errHost)
		_ = svc.Release(ctx, reservation.ID, releaseReason("upstream_error", errHost))
		if isAuthConcurrencyError(errHost) {
			return errorEnvelope("limit_rejected", errHost.Error()), nil
		}
		return errorEnvelope("upstream_error", errHost.Error()), nil
	}
	if status >= 400 {
		// Upstream executed; settle conservatively unless body has usage.
		hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "upstream_status",
			fmt.Errorf("upstream status %d: %s", status, truncateText(string(hostBody), 300)))
		parsed := usageparse.FromResponseBody(hostBody, req.SourceFormat)
		if settleErr := svc.SettleFromUsage(ctx, reservation, plan, parsed, req.SourceFormat, metrics); settleErr != nil {
			_ = svc.Release(ctx, reservation.ID, "settle_failed")
		}
		return okEnvelope(pluginapi.ExecutorResponse{Payload: hostBody, Headers: headers})
	}
	parsed := usageparse.FromResponseBody(hostBody, firstNonEmpty(req.Format, req.SourceFormat))
	if settleErr := svc.SettleFromUsage(ctx, reservation, plan, parsed, firstNonEmpty(req.Format, req.SourceFormat), metrics); settleErr != nil {
		// Preserve the upstream response, but never silently retain a hold when
		// the final ledger write cannot complete.
		_ = svc.Release(ctx, reservation.ID, "settle_failed")
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: hostBody, Headers: headers})
}

func executeStream(raw []byte) ([]byte, error) {
	svc := service.Current()
	if svc == nil {
		return errorEnvelope("service_unavailable", "credit manager is not configured"), nil
	}
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required"), nil
	}
	lifecycleID := lifecycleIDFromHeaders(req.Headers)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("panic: %v", recovered))
			}
		}()
		if err := runStream(context.Background(), svc, req, streamID, lifecycleID); err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
		closePluginStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func runStream(ctx context.Context, svc *service.Service, req rpcExecutorRequest, pluginStreamID, lifecycleID string) error {
	defer clearStreamLifecycle(lifecycleID)
	key, _, err := svc.ResolveIdentity(ctx, req.Headers, req.Metadata)
	if err != nil {
		return err
	}
	body := requestBody(req.ExecutorRequest)
	plan, err := svc.BuildReservePlan(ctx, req.Model, body)
	if err != nil {
		return err
	}
	auth := executorAuthIdentity(req.ExecutorRequest)
	reservation, err := svc.ReserveWithOptions(ctx, key, plan, "", service.ReserveOptions{
		Fallback: service.IsAPIProviderAuth(auth.Provider, auth.AuthID),
	})
	if err != nil {
		return err
	}
	if bindStreamLifecycle(lifecycleID, reservation.ID) {
		if releaseErr := svc.Release(ctx, reservation.ID, "client_disconnected_before_upstream"); releaseErr != nil {
			// Do not strand the Key slot if SQLite's final release briefly fails.
			// The financial hold remains recoverable through the normal stale path.
			_ = svc.FinishClientExecution(ctx, reservation.ID)
			return fmt.Errorf("release pre-upstream cancellation: %w", releaseErr)
		}
		return errClientDisconnectedBeforeUpstream
	}
	svc.TrackAuthCaptureWithAuth(reservation.ID, auth, plan.Model, req.Model)
	defer func() { _ = svc.FinishExecution(ctx, reservation.ID) }()
	if err := admitExecutorAuth(ctx, svc, reservation.ID, auth); err != nil {
		_ = svc.Release(ctx, reservation.ID, "auth_concurrency:"+err.Error())
		return err
	}
	if !beginStreamUpstream(lifecycleID) {
		if releaseErr := svc.Release(ctx, reservation.ID, "client_disconnected_before_upstream"); releaseErr != nil {
			_ = svc.FinishClientExecution(ctx, reservation.ID)
			return fmt.Errorf("release pre-upstream cancellation: %w", releaseErr)
		}
		return errClientDisconnectedBeforeUpstream
	}
	stopHeartbeat := startReservationHeartbeat(svc, reservation.ID)
	defer stopHeartbeat()

	startedAt := time.Now()
	body = requestBodyWithStreamUsage(body, req.SourceFormat, req.Format)
	body = prepareCompatBody(req, body)
	for attempt := 0; ; attempt++ {
		stream, err := openUpstreamStream(req, body)
		if err != nil {
			hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "host_execute_stream", err)
			_ = svc.Release(ctx, reservation.ID, releaseReason("upstream_stream_error", err))
			return err
		}
		initialCompletedAt := time.Now()
		if stream.StatusCode >= 400 {
			hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "upstream_stream_status",
				fmt.Errorf("upstream stream status %d", stream.StatusCode))
			_ = closeHostModelStream(stream.StreamID)
			parsed := usageparse.Result{}
			if settleErr := svc.SettleFromUsage(ctx, reservation, plan, parsed, req.SourceFormat,
				usageMetricsFromStream(body, startedAt, time.Time{}, initialCompletedAt, "failed")); settleErr != nil {
				_ = svc.Release(ctx, reservation.ID, "settle_failed")
			}
			return fmt.Errorf("host model status %d", stream.StatusCode)
		}
		if strings.TrimSpace(stream.StreamID) == "" {
			_ = svc.Release(ctx, reservation.ID, "empty_stream_id")
			return fmt.Errorf("empty host stream id")
		}
		verdict, terminalErr := pumpUpstreamStream(ctx, svc, req, body, stream, pluginStreamID, reservation, plan, startedAt)
		_ = closeHostModelStream(stream.StreamID)
		switch {
		case verdict == upstreamVerdictRetryStrip:
			// Nothing was emitted downstream (the pump only returns this verdict
			// while no chunk has been forwarded), so the same request can be retried
			// on the same credential with the account-bound reasoning removed.
			hostLog("warn", "credit-manager retrying without encrypted reasoning: model="+req.Model,
				map[string]any{"model": req.Model, "provider": req.AuthProvider}, req.HostCallbackID)
			markCompatPoisonedReasoning(compatRequestKey(req.Headers), req.Model)
			var count int
			body, count = stripCompatEncryptedReasoning(body)
			if count == 0 || attempt >= 1 {
				hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "upstream_stream_terminal",
					errors.New(stripRetryFailedText))
				_ = svc.Release(ctx, reservation.ID, releaseReason("upstream_stream_error", errors.New(stripRetryFailedText)))
				return errors.New(stripRetryFailedText)
			}
			hostLogCompatFallback(req.HostCallbackID, req.Model, req.AuthProvider,
				[]string{strconv.Itoa(count) + "x encrypted_reasoning:stripped"})
			continue
		case verdict == upstreamVerdictSettled:
			return nil
		default:
			return terminalErr
		}
	}
}

// upstreamVerdict classifies how the upstream stream ended for the caller.
type upstreamVerdict int

const (
	upstreamVerdictSettled upstreamVerdict = iota
	upstreamVerdictRetryStrip
	upstreamVerdictFailed
)

// stripRetryFailedText is what the client sees when the stripped retry did not
// rescue the request either.
const stripRetryFailedText = "upstream rejected the replayed reasoning state and the retry without it did not succeed"

// stripEncryptedRetryText is the exact upstream verdict this retry exists for.
const stripEncryptedRetryText = "thinking_signature_invalid"

// openUpstreamStream asks the host to execute the request upstream and returns
// the stream handle it readies.
func openUpstreamStream(req rpcExecutorRequest, body []byte) (pluginapi.HostModelStreamResponse, error) {
	raw, err := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: firstNonEmpty(req.SourceFormat, "openai"),
			ExitProtocol:  firstNonEmpty(req.SourceFormat, req.Format, "openai"),
			Model:         strings.TrimSpace(req.Model),
			Stream:        true,
			Body:          body,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		ForcedProvider: upstreamForcedProvider(req.ExecutorRequest),
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return pluginapi.HostModelStreamResponse{}, err
	}
	var stream pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &stream); err != nil {
		return pluginapi.HostModelStreamResponse{}, err
	}
	return stream, nil
}

// pumpUpstreamStream reads the upstream stream to its end, forwarding every chunk
// downstream and settling the reservation the way the attempt ended.
//
// It reports upstreamVerdictRetryStrip only while no chunk has been forwarded: an
// account-bound reasoning failure that arrives before the client has seen
// anything can be retried invisibly, one that arrives after cannot.
func pumpUpstreamStream(
	ctx context.Context,
	svc *service.Service,
	req rpcExecutorRequest,
	body []byte,
	stream pluginapi.HostModelStreamResponse,
	pluginStreamID string,
	reservation store.Reservation,
	plan service.ReservePlan,
	startedAt time.Time,
) (upstreamVerdict, error) {
	firstChunkAt := time.Time{}
	completedAt := time.Time{}
	var buffer bytes.Buffer
	maxBuffer := svc.Config().Stream.MaxBufferBytes
	terminal := newStreamTerminalDetector(body)
	emitted := false
	for {
		chunkRaw, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: stream.StreamID})
		if errRead != nil {
			completedAt = time.Now()
			hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "host_stream_read", errRead)
			parsed := parseExecutorStreamUsage(buffer.Bytes(), req)
			if settleErr := svc.SettleFromUsage(ctx, reservation, plan, parsed, firstNonEmpty(req.SourceFormat, req.Format),
				usageMetricsFromStream(body, startedAt, firstChunkAt, completedAt, "failed")); settleErr != nil {
				_ = svc.Release(ctx, reservation.ID, "settle_failed")
			}
			return upstreamVerdictFailed, errRead
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			completedAt = time.Now()
			hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "host_stream_chunk_decode", err)
			if settleErr := svc.SettleFromUsage(ctx, reservation, plan, usageparse.Result{}, req.SourceFormat,
				usageMetricsFromStream(body, startedAt, firstChunkAt, completedAt, "failed")); settleErr != nil {
				_ = svc.Release(ctx, reservation.ID, "settle_failed")
			}
			return upstreamVerdictFailed, err
		}
		if chunk.Error != "" {
			completedAt = time.Now()
			// This text is what the client displays, and the plugin is the only
			// layer that sees it: the in-stream terminal failure of an attempt the
			// host already retried. Log it before settling.
			hostLogRequestFailure(req.HostCallbackID, req.Model, req.AuthProvider, "upstream_stream_terminal", errors.New(chunk.Error))
			if !emitted && isEncryptedReasoningFailure(chunk.Error) {
				return upstreamVerdictRetryStrip, errors.New(chunk.Error)
			}
			parsed := parseExecutorStreamUsage(buffer.Bytes(), req)
			if settleErr := svc.SettleFromUsage(ctx, reservation, plan, parsed, firstNonEmpty(req.SourceFormat, req.Format),
				usageMetricsFromStream(body, startedAt, firstChunkAt, completedAt, "failed")); settleErr != nil {
				_ = svc.Release(ctx, reservation.ID, "settle_failed")
			}
			return upstreamVerdictFailed, fmt.Errorf("%s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if terminal.Feed(chunk.Payload) {
				// A failed optimization must not hide completion from the client or
				// interrupt the financial settlement performed below.
				_ = svc.FinishExecution(ctx, reservation.ID)
			}
			if buffer.Len() < maxBuffer {
				remain := min(maxBuffer-buffer.Len(), len(chunk.Payload))
				_, _ = buffer.Write(chunk.Payload[:remain])
			}
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			if err := emitPluginStreamChunk(pluginStreamID, bytes.Clone(chunk.Payload)); err != nil {
				completedAt = time.Now()
				parsed := parseExecutorStreamUsage(buffer.Bytes(), req)
				if settleErr := svc.SettleFromUsage(ctx, reservation, plan, parsed, firstNonEmpty(req.SourceFormat, req.Format),
					usageMetricsFromStream(body, startedAt, firstChunkAt, completedAt, "cancelled")); settleErr != nil {
					_ = svc.Release(ctx, reservation.ID, "settle_failed")
				}
				return upstreamVerdictFailed, err
			}
			emitted = true
		}
		if chunk.Done {
			break
		}
	}
	completedAt = time.Now()
	parsed := parseExecutorStreamUsage(buffer.Bytes(), req)
	if err := svc.SettleFromUsage(ctx, reservation, plan, parsed, firstNonEmpty(req.SourceFormat, req.Format),
		usageMetricsFromStream(body, startedAt, firstChunkAt, completedAt, "success")); err != nil {
		_ = svc.Release(ctx, reservation.ID, "settle_failed")
		return upstreamVerdictFailed, err
	}
	return upstreamVerdictSettled, nil
}

// isEncryptedReasoningFailure reports whether this upstream text is the
// account-bound reasoning rejection the stripped retry exists for.
func isEncryptedReasoningFailure(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, stripEncryptedRetryText) {
		return true
	}
	return strings.Contains(lower, "encrypted content") && strings.Contains(lower, "could not be verified")
}

func parseExecutorStreamUsage(buf []byte, req rpcExecutorRequest) usageparse.Result {
	var best usageparse.Result
	for _, format := range []string{req.SourceFormat, req.Format, "openai-response", "openai", "codex"} {
		format = strings.TrimSpace(format)
		if format == "" {
			continue
		}
		got := usageparse.FromStreamBuffer(buf, format)
		if got.Found && !got.Partial {
			return got
		}
		if got.Found && !best.Found {
			best = got
		}
	}
	if !best.Found {
		best = usageparse.FromResponseBody(buf, firstNonEmpty(req.SourceFormat, req.Format))
	}
	return best
}

// executorAuthIdentity is the credential the host dispatched this attempt to.
func executorAuthIdentity(req pluginapi.ExecutorRequest) store.AuthIdentity {
	return store.AuthIdentity{
		AuthID:    strings.TrimSpace(req.AuthID),
		Provider:  strings.TrimSpace(req.AuthProvider),
		AuthIndex: firstNonEmpty(metadataString(req.Metadata, "selected_auth_index"), metadataString(req.Metadata, "auth_index")),
	}
}

func admitExecutorAuth(ctx context.Context, svc *service.Service, reservationID string, auth store.AuthIdentity) error {
	if auth.Empty() {
		return nil
	}
	return svc.AdmitAuth(ctx, reservationID, auth)
}

func isAuthConcurrencyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrConcurrentLimit) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "maximum concurrent") || strings.Contains(msg, "limit_rejected")
}

func startReservationHeartbeat(svc *service.Service, reservationID string) func() {
	done := make(chan struct{})
	var stopOnce sync.Once
	if svc == nil || strings.TrimSpace(reservationID) == "" {
		return func() { stopOnce.Do(func() { close(done) }) }
	}
	_ = svc.TouchReservation(context.Background(), reservationID)
	interval := time.Minute
	if timeout := svc.Config().Stream.StaleReservationTimeout; timeout > 0 && timeout/3 < interval {
		interval = timeout / 3
	}
	if interval < time.Second {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = svc.TouchReservation(context.Background(), reservationID)
			}
		}
	}()
	return func() { stopOnce.Do(func() { close(done) }) }
}

func hostModelExecute(hostCallbackID string, req pluginapi.ExecutorRequest, body []byte, stream bool) ([]byte, http.Header, int, error) {
	raw, err := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: firstNonEmpty(req.SourceFormat, "openai"),
			ExitProtocol:  firstNonEmpty(req.Format, req.SourceFormat, "openai"),
			Model:         strings.TrimSpace(req.Model),
			Stream:        stream,
			Body:          body,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		ForcedProvider: upstreamForcedProvider(req),
		HostCallbackID: hostCallbackID,
	})
	if err != nil {
		return nil, nil, 0, err
	}
	var resp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, 0, err
	}
	return resp.Body, resp.Headers, resp.StatusCode, nil
}

// prepareCompatBody rewrites a request body for the provider that will serve it.
//
// Two rewrites live here, both driven by the same judgment that the attempt can
// reach an OpenAI-compatible API provider:
//
//   - account-bound reasoning state is stripped when this credential and model
//     already failed hydration once, because the client will keep replaying items
//     only the account that produced them can decrypt;
//   - structured-output declarations are downgraded, and the thinking-mode
//     reasoning field is supplied, for the providers that reject them.
//
// The host may also move an attempt onto such a provider on its own, without the
// plugin picking one, so a model whose scheduling window offered an API provider
// is prepared as well.
func prepareCompatBody(req rpcExecutorRequest, body []byte) []byte {
	rewritten := body
	var stripped []string
	// The remembered failure is checked before the provider judgment: it is
	// evidence that this replay genuinely carried state another account could not
	// hydrate, which is stronger than any inference about the candidate window.
	if compatReasoningPoisoned(compatRequestKey(req.Headers), req.Model) {
		if strippedBody, count := stripCompatEncryptedReasoning(rewritten); count > 0 {
			rewritten = strippedBody
			stripped = append(stripped, strconv.Itoa(count)+"x encrypted_reasoning:stripped")
		}
	}
	if compatTargetsAPIProvider(req.ExecutorRequest) {
		sanitizedBody, sanitized := sanitizeCompatRequestBody(rewritten)
		rewritten = sanitizedBody
		stripped = append(stripped, sanitized...)
	}
	hostLogCompatFallback(req.HostCallbackID, req.Model, req.AuthProvider, stripped)
	return rewritten
}

// upstreamForcedProvider pins the inner host execution to the provider of the
// credential this plugin selected. Without it the host's own selector may move
// the request to a different provider, which both bypasses the key's binding and
// hands an API provider a body it cannot accept.
//
// It is limited to the codex/API-provider pair this plugin routes between: other
// providers (Claude, Gemini, Antigravity) have their own routing and are left
// exactly as they were.
func upstreamForcedProvider(req pluginapi.ExecutorRequest) string {
	provider := strings.TrimSpace(req.AuthProvider)
	lower := strings.ToLower(provider)
	if lower == "codex" || strings.HasPrefix(lower, "openai-compatible") {
		return provider
	}
	return ""
}

func requestAuthID(req pluginapi.ExecutorRequest) string {
	if authID := strings.TrimSpace(req.AuthID); authID != "" {
		return authID
	}
	return metadataString(req.Metadata, "selected_auth_id")
}

// releaseReason keeps the original release code as a prefix and appends the
// upstream error, so the console's released-failure list names the actual error
// instead of a generic bucket.
func releaseReason(code string, err error) string {
	code = strings.TrimSpace(code)
	if err == nil {
		return code
	}
	detail := errorText(err)
	if detail == "" {
		return code
	}
	return truncateText(code+": "+detail, maxReleaseReasonBytes)
}

const maxReleaseReasonBytes = 400

func emitPluginStreamChunk(streamID string, payload []byte) error {
	_, err := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func closePluginStream(streamID, errMsg string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)})
}

func closeHostModelStream(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, err := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	return err
}
