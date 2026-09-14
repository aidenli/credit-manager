package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStreamLifecycleCancellationReleasesOnlyKeySlot(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "lifecycle", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store().UpdatePluginKeyPolicy(ctx, store.PluginKeyPolicyUpdate{ID: key.ID, MaxConcurrentRequests: int64Ptr(1)}); err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Store().Reserve(ctx, store.ReserveRequest{
		CallerID:             key.CallerID,
		PluginKeyID:          key.ID,
		IdempotencyKey:       "first",
		Model:                "model",
		RequestTokenEstimate: 1,
		AmountMicroUSD:       money.MicroUSD(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "request-cancelled"
	trackStreamLifecycle(requestID, svc)
	if bindStreamLifecycle(requestID, reservation.ID) {
		t.Fatal("request was canceled before binding")
	}
	completeStreamLifecycle(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionCanceled})

	overview, err := svc.Store().GetKeyUsageOverview(ctx, key.ID, reservation.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if overview.ActiveReservations != 0 {
		t.Fatalf("active key slots = %d, want 0", overview.ActiveReservations)
	}
	got, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldAmountMicroUSD != reservation.HeldMicroUSD {
		t.Fatalf("financial hold = %d, want %d", got.HeldAmountMicroUSD, reservation.HeldMicroUSD)
	}
	if _, err := svc.Store().Reserve(ctx, store.ReserveRequest{
		CallerID: key.CallerID, PluginKeyID: key.ID, IdempotencyKey: "second", Model: "model", RequestTokenEstimate: 1, AmountMicroUSD: money.MicroUSD(1),
	}); err != nil {
		t.Fatalf("next request was still blocked by canceled client: %v", err)
	}
}

func TestStreamLifecycleCancellationBeforeReservationSkipsUpstream(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{ID: "lifecycle", MatchKind: store.MatchGlob, Pattern: "*", Priority: 10, Enabled: true, Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	key, material, err := svc.MintKey(ctx, service.BootstrapCallerID, "before-upstream", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "request-cancelled-before-reserve"
	trackStreamLifecycle(requestID, svc)
	completeStreamLifecycle(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionCanceled})
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	calledHost := false
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		calledHost = true
		return nil, 0, fmt.Errorf("host call must not start after pre-upstream cancellation: %s", method)
	}
	body := []byte(`{"model":"before-upstream","stream":true,"max_tokens":1,"messages":[{"role":"user","content":"test"}]}`)
	err = runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "before-upstream", SourceFormat: "openai", Format: "openai", Stream: true,
		Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}}, Payload: body,
	}}, "downstream", requestID)
	if !errors.Is(err, errClientDisconnectedBeforeUpstream) {
		t.Fatalf("runStream error = %v", err)
	}
	if calledHost {
		t.Fatal("pre-reservation cancellation started a nested host request")
	}
	got, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldAmountMicroUSD != 0 {
		t.Fatalf("never-started upstream left financial hold = %d", got.HeldAmountMicroUSD)
	}
}

func TestStreamLifecycleCancellationAfterBindStopsLaunch(t *testing.T) {
	svc := newLifecycleTestService(t)
	const requestID = "request-cancelled-after-bind"
	trackStreamLifecycle(requestID, svc)
	if bindStreamLifecycle(requestID, "reservation") {
		t.Fatal("unexpected cancellation before bind")
	}
	completeStreamLifecycle(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionCanceled})
	if beginStreamUpstream(requestID) {
		t.Fatal("cancellation after bind still allowed upstream launch")
	}
	clearStreamLifecycle(requestID)
}

func TestImageCompletionReleasesHoldWhenSettlementFails(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "image-settle-failure", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Store().Reserve(ctx, store.ReserveRequest{
		CallerID: key.CallerID, PluginKeyID: key.ID, IdempotencyKey: "image-settle-failure", Model: "image-model", RequestTokenEstimate: 1, AmountMicroUSD: money.MicroUSD(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "image-settle-failure"
	imageHoldsMu.Lock()
	imageHolds[requestID] = imageHold{reservation: reservation, plan: service.ReservePlan{
		Model: "image-model", ImageCount: 1,
		Price: money.PricePerMTok{BillingMode: money.BillingPerImage, PerImage: -1},
	}}
	imageHoldsMu.Unlock()
	t.Cleanup(func() {
		imageHoldsMu.Lock()
		delete(imageHolds, requestID)
		imageHoldsMu.Unlock()
	})
	raw, err := json.Marshal(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeInterceptedRequest(raw); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldAmountMicroUSD != 0 {
		t.Fatalf("settle failure leaked image hold = %d", got.HeldAmountMicroUSD)
	}
	final, err := svc.Store().GetReservation(ctx, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.ReservationReleased {
		t.Fatalf("reservation status = %s, want released", final.Status)
	}
}

func TestAfterAuthCarriesAndExecutorStripsLifecycleHeader(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	_, material, err := svc.MintKey(ctx, service.BootstrapCallerID, "header", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := pluginapi.RequestInterceptRequest{
		RequestID: "request-header", Stream: true, Model: "model",
		Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	responseRaw, err := interceptRequestAfterAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(responseRaw, &env); err != nil || !env.OK {
		t.Fatalf("interceptor envelope = %+v err=%v", env, err)
	}
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Headers.Get(lifecycleRequestHeader); got != request.RequestID {
		t.Fatalf("lifecycle header = %q, want %q", got, request.RequestID)
	}
	request.Headers.Set(lifecycleRequestHeader, response.Headers.Get(lifecycleRequestHeader))
	if got := lifecycleIDFromHeaders(request.Headers); got != request.RequestID {
		t.Fatalf("extracted lifecycle id = %q", got)
	}
	if got := request.Headers.Get(lifecycleRequestHeader); got != "" {
		t.Fatalf("private lifecycle header leaked to nested host request: %q", got)
	}
	clearStreamLifecycle(request.RequestID)
}

func newLifecycleTestService(t *testing.T) *service.Service {
	t.Helper()
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(func() {
		service.Shutdown()
		streamLifecyclesMu.Lock()
		streamLifecycles = map[string]*streamLifecycle{}
		streamLifecyclesMu.Unlock()
	})
	return svc
}

func int64Ptr(value int64) *int64 { return &value }
