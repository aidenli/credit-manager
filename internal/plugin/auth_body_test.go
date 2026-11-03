package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestFrontendAuthenticateDoesNotDependOnBody(t *testing.T) {
	svc := newLifecycleTestService(t)
	key, material, err := svc.MintKey(context.Background(), service.BootstrapCallerID, "body-independent", 1_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, token, principal string
	}{
		{"valid key", "POST", "/v1/responses", material.Plaintext, key.Principal},
		{"image key", "POST", "/v1/images/generations", material.Plaintext, key.Principal},
		{"missing key", "POST", "/v1/responses", "", ""},
		{"invalid key", "POST", "/v1/responses", "invalid", ""},
		{"public models", "GET", "/v1/models", "", "credit-manager:model-directory"},
		{"public gemini models", "GET", "/v1beta/models", "", "credit-manager:model-directory"},
		{"models write needs key", "POST", "/v1/models", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var baseline pluginapi.FrontendAuthResponse
			for i, body := range []any{nil, []byte(`{"model":"ignored","n":3}`), []byte("not JSON\x00")} {
				request := map[string]any{"Method": tc.method, "Path": tc.path, "Headers": http.Header{"Authorization": []string{"Bearer " + tc.token}}}
				if body != nil {
					request["Body"] = body
					request["DecodedBody"] = body
				}
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				response, err := HandleMethod(pluginabi.MethodFrontendAuthAuthenticate, raw)
				if err != nil {
					t.Fatal(err)
				}
				var env envelope
				if err := json.Unmarshal(response, &env); err != nil || !env.OK {
					t.Fatalf("auth envelope = %s, err = %v", response, err)
				}
				var auth pluginapi.FrontendAuthResponse
				if err := json.Unmarshal(env.Result, &auth); err != nil {
					t.Fatal(err)
				}
				if auth.Authenticated != (tc.principal != "") || auth.Principal != tc.principal {
					t.Fatalf("auth = %+v, want principal %q", auth, tc.principal)
				}
				if i == 0 {
					baseline = auth
				} else if !reflect.DeepEqual(auth, baseline) {
					t.Fatalf("body changed authentication: got %+v, without body %+v", auth, baseline)
				}
			}
		})
	}
}

func TestAfterAuthPreservesImageBodyReservation(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "image-body", MatchKind: store.MatchExact, Pattern: "gpt-image-1", Priority: 100, Enabled: true,
		Price: money.PricePerMTok{BillingMode: money.BillingPerImage, PerImage: 40_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, material, err := svc.MintKey(ctx, service.BootstrapCallerID, "image-body", 1_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "image-body-reservation"
	t.Cleanup(func() {
		raw, _ := json.Marshal(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionCanceled})
		if _, err := HandleMethod(pluginabi.MethodRequestComplete, raw); err != nil {
			t.Error(err)
		}
	})
	raw, err := json.Marshal(pluginapi.RequestInterceptRequest{
		RequestID: requestID, SourceFormat: "openai-image", Model: "gpt-image-1",
		Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}},
		Body:    []byte(`{"model":"gpt-image-1","n":3,"prompt":"cat"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := HandleMethod(pluginabi.MethodRequestInterceptAfter, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(response, &env); err != nil || !env.OK {
		t.Fatalf("after-auth envelope = %s, err = %v", response, err)
	}
	var intercepted pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &intercepted); err != nil {
		t.Fatal(err)
	}
	if intercepted.Terminate || len(intercepted.Body) != 0 {
		t.Fatalf("image request terminated or replaced: %+v", intercepted)
	}
	imageHoldsMu.Lock()
	hold, ok := imageHolds[requestID]
	imageHoldsMu.Unlock()
	if !ok || hold.plan.ImageCount != 3 || hold.plan.Amount != 120_000 || hold.reservation.HeldMicroUSD != 120_000 {
		t.Fatalf("image hold = %+v, exists = %v; want 3 images / 120000 microUSD", hold, ok)
	}
	got, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldAmountMicroUSD != 120_000 {
		t.Fatalf("key hold = %d, want 120000", got.HeldAmountMicroUSD)
	}
}
