package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
)

var admissionTestSequence atomic.Uint64

func decodeAdmissionResponse(t *testing.T, raw []byte, err error) pluginapi.RequestInterceptResponse {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope=%s err=%v", raw, err)
	}
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func beforeAdmissionTest(t *testing.T, id string, headers http.Header) http.Header {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"RequestID": id, "Headers": headers, "Body": "NOT BASE64", "DecodedBody": 42})
	out, err := interceptRequestBeforeAuth(raw)
	response := decodeAdmissionResponse(t, out, err)
	if response.Terminate {
		t.Fatalf("before rejected: %+v", response)
	}
	result := headers.Clone()
	if result == nil {
		result = make(http.Header)
	}
	for _, name := range response.ClearHeaders {
		result.Del(name)
	}
	for name, values := range response.Headers {
		result[name] = values
	}
	return result
}

// Existing scheduler policy tests must enter through the real beforeAuth bridge.
func pickAuthWithLifecycleTest(t *testing.T, raw []byte) ([]byte, error) {
	t.Helper()
	if !service.Current().Config().AuthOnly {
		return pickAuth(raw)
	}
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("scheduler-test-%d", admissionTestSequence.Add(1))
	req.Options.Headers = beforeAdmissionTest(t, id, http.Header(req.Options.Headers))
	defer service.Current().CompleteAuthAdmission(id)
	raw, _ = json.Marshal(req)
	return pickAuth(raw)
}

func pickAdmissionTest(t *testing.T, headers http.Header, selection string, candidates ...pluginapi.SchedulerAuthCandidate) schedulerPickResponse {
	t.Helper()
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Model: "model", Candidates: candidates, Options: pluginapi.SchedulerOptions{Headers: headers, Metadata: map[string]any{"selection_id": selection}}})
	out, err := pickAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	var result schedulerPickResponse
	if json.Unmarshal(out, &env) != nil || !env.OK || json.Unmarshal(env.Result, &result) != nil {
		t.Fatalf("pick=%s", out)
	}
	return result
}

func preparedAdmissionTest(t *testing.T, id string, headers http.Header) http.Header {
	t.Helper()
	headers = beforeAdmissionTest(t, id, headers)
	if response := pickAdmissionTest(t, headers, "selection-"+id, pluginapi.SchedulerAuthCandidate{ID: "test-account", Provider: "codex"}); response.Reject || response.AuthID == "" {
		t.Fatalf("pick=%+v", response)
	}
	return headers
}

func TestAuthOnlyAdmissionLifecycle(t *testing.T) {
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir, cfg.AuthOnly = t.TempDir(), true
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(service.Shutdown)
	key, material, err := svc.MintKey(context.Background(), service.BootstrapCallerID, "admission", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := svc.Store().UpsertAuthConcurrencyLimit(context.Background(), "codex", id, 1); err != nil {
			t.Fatal(err)
		}
	}
	headers := http.Header{"Authorization": {"Bearer " + material.Plaintext}, authAdmissionHeader: {"spoof"}, authWarmupHeader: {"spoof"}}
	a := pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex", Priority: 2}
	b := pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "codex", Priority: 1}
	h1 := beforeAdmissionTest(t, "one", headers)
	if h1.Get(authAdmissionHeader) != "one" || h1.Get(authWarmupHeader) != "" {
		t.Fatal("untrusted headers survived")
	}
	if p := pickAdmissionTest(t, h1, "pick-1", a, b); p.AuthID != "a" {
		t.Fatalf("first=%+v", p)
	}
	if p := pickAdmissionTest(t, h1, "pick-1", a, b); p.AuthID != "a" {
		t.Fatalf("duplicate=%+v", p)
	}
	h2 := beforeAdmissionTest(t, "two", headers)
	if p := pickAdmissionTest(t, h2, "pick-2", a, b); p.AuthID != "b" {
		t.Fatalf("alternative=%+v", p)
	}
	h3 := beforeAdmissionTest(t, "three", headers)
	if p := pickAdmissionTest(t, h3, "pick-3", a, b); !p.Reject || !p.Handled || p.RejectCode != "auth_unavailable" {
		t.Fatalf("full=%+v", p)
	}
	raw, _ := json.Marshal(map[string]any{"RequestID": "one", "Model": "model", "Headers": h1, "Metadata": map[string]any{"selected_auth_id": "a", "body": 42}, "Body": "NOT BASE64"})
	out, err := interceptRequestAfterAuth(raw)
	response := decodeAdmissionResponse(t, out, err)
	if response.Terminate || !strings.Contains(strings.Join(response.ClearHeaders, ","), authAdmissionHeader) {
		t.Fatalf("after=%+v", response)
	}
	// Per-attempt clone clearing must not mutate the scheduler's original headers.
	if h1.Get(authAdmissionHeader) != "one" {
		t.Fatal("retry token lost")
	}
	for range 2 {
		if _, err := completeInterceptedRequest([]byte(`{"RequestID":"two","Body":42}`)); err != nil {
			t.Fatal(err)
		}
	}
	if p := pickAdmissionTest(t, h1, "retry", b); p.AuthID != "b" {
		t.Fatalf("retry switch=%+v", p)
	}
	if p := pickAdmissionTest(t, h3, "pick-4", a); p.AuthID != "a" {
		t.Fatalf("old account not released=%+v", p)
	}
	// Unknown/missing tokens fail closed even with unlimited native candidates.
	for _, h := range []http.Header{nil, {authAdmissionHeader: {"unknown"}}} {
		if p := pickAdmissionTest(t, h, "unknown", a); !p.Reject {
			t.Fatalf("unknown=%+v", p)
		}
	}
	// Mismatched actual selection rejects and always removes internal headers.
	raw, _ = json.Marshal(map[string]any{"RequestID": "one", "Headers": h1, "Metadata": map[string]any{"selected_auth_id": "a"}})
	out, err = interceptRequestAfterAuth(raw)
	response = decodeAdmissionResponse(t, out, err)
	if !response.Terminate || len(response.ClearHeaders) == 0 {
		t.Fatalf("mismatch=%+v", response)
	}
	if _, err := completeInterceptedRequest([]byte(`{"RequestID":"three"}`)); err != nil { t.Fatal(err) }
	// Native credentials keep the existing caller-scope identity policy, but
	// managed accounts still acquire an atomic scheduler admission.
	native := beforeAdmissionTest(t, "native", http.Header{"Authorization": {"Bearer native-host-key"}})
	if p := pickAdmissionTest(t, native, "native-pick", a); p.AuthID != "a" { t.Fatalf("native=%+v", p) }
	raw, _ = json.Marshal(map[string]any{"RequestID":"native", "Headers":native, "Model":"model", "Metadata":map[string]any{"selected_auth_id":"a", "caller_scope":key.CallerScope}})
	out, err = interceptRequestAfterAuth(raw)
	if resp := decodeAdmissionResponse(t,out,err); resp.Terminate { t.Fatalf("mapped native rejected: %+v",resp) }
	native2 := beforeAdmissionTest(t,"native2",http.Header{"Authorization":{"Bearer native-host-key"}})
	if p:=pickAdmissionTest(t,native2,"native2-pick",a);!p.Reject { t.Fatalf("native cap bypass: %+v",p) }
	if _,err:=completeInterceptedRequest([]byte(`{"RequestID":"native"}`));err!=nil {t.Fatal(err)}
	if p:=pickAdmissionTest(t,native2,"native2-retry",a);p.AuthID!="a" {t.Fatalf("native release: %+v",p)}
	raw,_=json.Marshal(map[string]any{"RequestID":"native2","Headers":native2,"Metadata":map[string]any{"selected_auth_id":"a"}})
	out,err=interceptRequestAfterAuth(raw)
	if resp:=decodeAdmissionResponse(t,out,err);!resp.Terminate || resp.StatusCode!=http.StatusUnauthorized {t.Fatalf("unmapped native policy changed: %+v",resp)}
	for _, table := range []string{"reservations", "usage_ledger"} {
		var count int
		if err := svc.Store().DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s=%d %v", table, count, err)
		}
	}
}
