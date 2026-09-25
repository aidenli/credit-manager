package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// The probe must pin the account it was asked to test and ask both questions.
// The pin travels as the warmup nonce, which the scheduler answers with that
// exact account or rejects - that is what "no fallback" means here.
func TestOAuthTestProbePinsAccountAndAsksBothQuestions(t *testing.T) {
	oldHost := HostCall
	defer func() { HostCall = oldHost }()

	var requests []hostModelExecutionRequest
	var nonces []string
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		var result any = map[string]any{}
		switch method {
		case pluginabi.MethodHostModelExecute:
			var req hostModelExecutionRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, 0, err
			}
			requests = append(requests, req)
			nonces = append(nonces, req.Headers.Get(authWarmupHeader))
			result = pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`),
			}
		default:
			return nil, 0, nil
		}
		encoded, err := okEnvelope(result)
		return encoded, 0, err
	}

	executor := hostOAuthTestExecutor{}
	answer, err := executor.ExecuteOAuthTest(context.Background(), service.OAuthTestRequest{
		RunID:             "run-1",
		Auth:              store.AuthIdentity{AuthID: "codex-a@example.com-pro.json", Provider: "codex"},
		Model:             "gpt-6-astra",
		ThinkingIntensity: "high",
		Prompt:            "自定义题1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("host calls = %d, want one per question", len(requests))
	}
	for i, req := range requests {
		if strings.TrimSpace(nonces[i]) == "" {
			t.Fatalf("call %d carried no warmup pin", i)
		}
		if req.Model != "gpt-6-astra" {
			t.Fatalf("call %d model = %q", i, req.Model)
		}
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatalf("call %d body is not JSON: %v", i, err)
		}
		if body["reasoning_effort"] != "high" {
			t.Fatalf("call %d reasoning_effort = %v", i, body["reasoning_effort"])
		}
		if _, ok := body["reasoning"]; ok {
			// A Responses-only field in a Chat Completions body makes strict
			// upstreams reject the probe for its shape.
			t.Fatalf("call %d sent the Responses-only reasoning object: %s", i, req.Body)
		}
	}
	if !strings.Contains(string(requests[0].Body), "自定义题1") {
		t.Fatalf("first question = %s", requests[0].Body)
	}
	if !strings.Contains(string(requests[1].Body), "鹈鹕骑自行车") {
		t.Fatalf("second question = %s", requests[1].Body)
	}
	if answer.Question1 != "ok" {
		t.Fatalf("question1 = %q", answer.Question1)
	}
	// The HTML is stored exactly as returned: the probe does not grade it, so a
	// non-SVG answer is recorded rather than failed.
	if answer.Question2HTML != "ok" {
		t.Fatalf("question2 = %q", answer.Question2HTML)
	}
}

// A busy account refuses the pick instead of queueing, so the probe waits and
// asks again. The nonce must be fresh per attempt: the lease behind the pin is
// finished after every attempt, so a reused nonce would expire mid-question.
func TestOAuthTestProbeWaitsWhileTheAccountIsBusy(t *testing.T) {
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	oldWait := oauthTestRetryWait
	oauthTestRetryWait = time.Millisecond
	defer func() { oauthTestRetryWait = oldWait }()

	var nonces []string
	calls := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		if method != pluginabi.MethodHostModelExecute {
			return nil, 0, nil
		}
		var req hostModelExecutionRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, 0, err
		}
		nonces = append(nonces, req.Headers.Get(authWarmupHeader))
		calls++
		if calls <= 2 {
			return nil, 0, errors.New(`host_call_failed: scheduler rejected auth pick plugin_id=credit-manager error=warmup target unavailable`)
		}
		encoded, err := okEnvelope(pluginapi.HostModelExecutionResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"choices":[{"message":{"content":"ok"}}]}`),
		})
		return encoded, 0, err
	}

	answer, err := hostOAuthTestExecutor{}.ExecuteOAuthTest(context.Background(), service.OAuthTestRequest{
		RunID: "run-busy",
		Auth:  store.AuthIdentity{AuthID: "codex-busy@example.com-pro.json", Provider: "codex"},
		Model: "gpt-6-astra",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Question 1 is rejected twice while the account is busy and succeeds on the
	// third attempt; question 2 then takes one call of its own.
	if calls != 4 {
		t.Fatalf("host calls = %d, want 4 (two busy rejections then one call per question)", calls)
	}
	if answer.Question1 != "ok" || answer.Question2HTML != "ok" {
		t.Fatalf("answer = %#v", answer)
	}
	seen := map[string]bool{}
	for _, nonce := range nonces {
		if strings.TrimSpace(nonce) == "" {
			t.Fatal("an attempt carried no pin")
		}
		if seen[nonce] {
			t.Fatal("a retry reused the previous nonce, which would expire the pin")
		}
		seen[nonce] = true
	}
}

// Any other failure is this account's result: the probe does not hide it behind
// retries, because the operator reads the reason later.
func TestOAuthTestProbeDoesNotRetryRealFailures(t *testing.T) {
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	oldWait := oauthTestRetryWait
	oauthTestRetryWait = time.Millisecond
	defer func() { oauthTestRetryWait = oldWait }()

	calls := 0
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		if method != pluginabi.MethodHostModelExecute {
			return nil, 0, nil
		}
		calls++
		return nil, 0, errors.New("host_call_failed: 上游返回 HTTP 400 bad request")
	}
	if _, err := (hostOAuthTestExecutor{}).ExecuteOAuthTest(context.Background(), service.OAuthTestRequest{
		RunID: "run-fail",
		Auth:  store.AuthIdentity{AuthID: "codex-fail@example.com-pro.json", Provider: "codex"},
		Model: "gpt-6-astra",
	}); err == nil {
		t.Fatal("a rejected question must surface as this account's failure")
	}
	if calls != 1 {
		t.Fatalf("host calls = %d, want no retry for a non-capacity failure", calls)
	}
}

func TestExtractOAuthTestTextReadsEveryResponseShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"chat completions", `{"choices":[{"message":{"content":"石破茂"}}]}`, "石破茂"},
		{"legacy completion", `{"choices":[{"text":"石破茂"}]}`, "石破茂"},
		{"responses output", `{"output":[{"content":[{"type":"output_text","text":"石破"}]},{"content":[{"text":"茂"}]}]}`, "石破茂"},
		{"output_text", `{"output_text":"石破茂"}`, "石破茂"},
		{"streamed anyway", "data: {\"choices\":[{\"delta\":{\"content\":\"石\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"破\"}}]}\n\ndata: [DONE]\n", "石破"},
		{"noise is not an answer", `{"error":{"message":"boom"}}`, ""},
		{"plain body is not an answer", `<html>nope</html>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractOAuthTestText([]byte(tc.raw)); got != tc.want {
				t.Fatalf("extractOAuthTestText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeOAuthTestHTMLStripsCodeFence(t *testing.T) {
	fenced := "```html\n<svg><circle r=\"1\"/></svg>\n```"
	if got := normalizeOAuthTestHTML(fenced); got != `<svg><circle r="1"/></svg>` {
		t.Fatalf("fenced = %q", got)
	}
	plain := "  <svg></svg>  "
	if got := normalizeOAuthTestHTML(plain); got != "<svg></svg>" {
		t.Fatalf("plain = %q", got)
	}
}

// The warmup lease records what the probe spent, so a probe must report usage
// instead of a hardcoded zero.
func TestServiceTokenUsageReadsTheResponse(t *testing.T) {
	usage := serviceTokenUsage([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`))
	if usage.Input != 11 || usage.Output != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	if empty := serviceTokenUsage(nil); empty.Input != 0 || empty.Output != 0 {
		t.Fatalf("empty usage = %+v", empty)
	}
}
