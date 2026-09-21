package plugin

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWarmupTargetPinsOnlyRegisteredNonce(t *testing.T) {
	activeWarmups.Lock()
	activeWarmups.byNonce = map[string]*warmupLease{}
	activeWarmups.Unlock()
	lease := &warmupLease{
		Nonce: "nonce", Auth: store.AuthIdentity{AuthID: "auth-1", Provider: "claude"}, Model: "claude-haiku",
		StartedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute),
	}
	registerWarmupLease(lease)
	headers := make(http.Header)
	headers.Set(authWarmupHeader, "nonce")
	id, handled, err := warmupTarget(headers, []service.AuthPickCandidate{{ID: "other", Provider: "claude"}, {ID: "auth-1", Provider: "anthropic"}})
	if err != nil || !handled || id != "auth-1" {
		t.Fatalf("target = (%q, %t, %v)", id, handled, err)
	}
	headers.Set(authWarmupHeader, "unknown")
	if _, handled, err := warmupTarget(headers, []service.AuthPickCandidate{{ID: "other", Provider: "claude"}}); err != nil || handled {
		t.Fatalf("unknown nonce = handled:%t err:%v", handled, err)
	}
}

func TestWarmupExecutorTriesAnotherSelectedModelForSameAuth(t *testing.T) {
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	attempts := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		if method != pluginabi.MethodHostModelExecute {
			return nil, 0, errors.New("unexpected host call")
		}
		attempts++
		if attempts == 1 {
			return nil, 0, errors.New("warmup target unavailable")
		}
		encoded, err := okEnvelope(pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)})
		return encoded, 0, err
	}
	result, err := (hostAuthWarmupExecutor{}).ExecuteAuthWarmup(context.Background(), service.AuthWarmupRequest{
		RunID: "run", Auth: store.AuthIdentity{AuthID: "auth-1", Provider: "codex"}, Models: []string{"not-supported", "gpt-5.6-luna"}, EntryProtocol: "openai", RequestedAt: time.Now(),
	})
	if err != nil || attempts != 2 || result.Model != "gpt-5.6-luna" || result.Usage.Input != 2 || result.Usage.Output != 1 {
		t.Fatalf("result=%#v attempts=%d err=%v", result, attempts, err)
	}
}

func TestWarmupUsageIsNotEligibleForCustomerFallback(t *testing.T) {
	now := time.Now().UTC()
	activeWarmups.Lock()
	activeWarmups.byNonce = map[string]*warmupLease{
		"nonce": {Nonce: "nonce", Auth: store.AuthIdentity{AuthID: "auth-1", Provider: "claude"}, Model: "claude-haiku", StartedAt: now.Add(-time.Second), CompletedAt: now.Add(time.Second), ExpiresAt: now.Add(time.Minute), Usage: money.TokenUsage{Input: 2, Output: 1}},
	}
	activeWarmups.Unlock()
	record := pluginapi.UsageRecord{AuthID: "auth-1", Provider: "anthropic", Model: "claude-haiku", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 1}}
	if !isWarmupUsage(record) {
		t.Fatal("warmup usage must be ignored by customer attribution")
	}
	record.Detail.OutputTokens = 2
	if isWarmupUsage(record) {
		t.Fatal("nearby customer usage with different tokens must not be ignored")
	}
}

func TestWarmupResponseErrorClassifiesMissingAPIKey(t *testing.T) {
	err := warmupResponseError(http.StatusUnauthorized, []byte(`{"error":"Missing API key"}`))
	if err == nil || err.Error() != "warmup request missing API key" {
		t.Fatalf("warmup response error = %v", err)
	}
	// The status must survive classification: it is the only way a run can record
	// whether the host refused the request or the account answered badly.
	var statusErr *service.AuthWarmupUpstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusUnauthorized {
		t.Fatalf("status = %#v", statusErr)
	}
}

// The whole gpt-image family is image-only, including releases that appeared after
// the previous allow-list was written.
func TestIsImageOnlyModelMatchesWholeGptImageFamily(t *testing.T) {
	for _, model := range []string{
		"gpt-image-1", "gpt-image-1.5", "gpt-image-2",
		"gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
		"GPT-IMAGE-2.5", " grok-imagine-image ", "grok-imagine-video",
	} {
		if !service.IsImageOnlyModel(model) {
			t.Fatalf("%q must be treated as image-only", model)
		}
	}
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.5", "gpt-6-astra", "codex-auto-review", ""} {
		if service.IsImageOnlyModel(model) {
			t.Fatalf("%q must not be treated as image-only", model)
		}
	}
}

// Image models cannot be warmed through the host's model-execute callback, so the
// run must skip them rather than report a failure for every account.
func TestWarmupSkipsImageOnlyModels(t *testing.T) {
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	attempts := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		if method != pluginabi.MethodHostModelExecute {
			return nil, 0, errors.New("unexpected host call")
		}
		attempts++
		encoded, err := okEnvelope(pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"prompt_tokens":2,"completion_tokens":1}}`)})
		return encoded, 0, err
	}
	result, err := (hostAuthWarmupExecutor{}).ExecuteAuthWarmup(context.Background(), service.AuthWarmupRequest{
		RunID: "run", Auth: store.AuthIdentity{AuthID: "auth-1", Provider: "codex"},
		Models: []string{"gpt-image-2.5", "gpt-image-2", "gpt-5.6-luna"}, EntryProtocol: "openai", RequestedAt: time.Now(),
	})
	if err != nil || attempts != 1 || result.Model != "gpt-5.6-luna" {
		t.Fatalf("result=%#v attempts=%d err=%v", result, attempts, err)
	}
}

// Selecting only image models is reported as unsupported, which the service records
// as "skipped" instead of "failed".
func TestWarmupReportsImageOnlySelectionAsUnsupported(t *testing.T) {
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	attempts := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		attempts++
		return nil, 0, errors.New("unexpected host call")
	}
	_, err := (hostAuthWarmupExecutor{}).ExecuteAuthWarmup(context.Background(), service.AuthWarmupRequest{
		RunID: "run", Auth: store.AuthIdentity{AuthID: "auth-1", Provider: "codex"},
		Models: []string{"gpt-image-2.5-sunburst"}, EntryProtocol: "openai", RequestedAt: time.Now(),
	})
	if !errors.Is(err, service.ErrAuthWarmupModelUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if attempts != 0 {
		t.Fatalf("image-only selection made %d host calls, want 0", attempts)
	}
}
