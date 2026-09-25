package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/usageparse"
)

// hostOAuthTestExecutor runs the intelligence probe through the host's own model
// execution path.
//
// It reuses the warmup lease handshake (a nonce in authWarmupHeader) to pin the
// request to one account. That matters more than it looks: the scheduler answers
// a warmup pin with that exact account or rejects the pick, so a probe can never
// be served by another account's fallback. The operator's rule - no fallback, a
// failure is a failure - is enforced by the scheduler, not by hope.
type hostOAuthTestExecutor struct{}

func (hostOAuthTestExecutor) ExecuteOAuthTest(ctx context.Context, request service.OAuthTestRequest) (service.OAuthTestAnswer, error) {
	if request.Auth.Empty() {
		return service.OAuthTestAnswer{}, errors.New("目标 OAuth 不可用")
	}
	question1, question2 := service.OAuthTestQuestions(request.Prompt)
	answer1, err := runOAuthTestPrompt(ctx, request, question1)
	if err != nil {
		return service.OAuthTestAnswer{}, fmt.Errorf("题目1失败: %w", err)
	}
	answer2, err := runOAuthTestPrompt(ctx, request, question2)
	if err != nil {
		return service.OAuthTestAnswer{}, fmt.Errorf("题目2失败: %w", err)
	}
	// The HTML is stored as returned. The probe does not grade the answer: the
	// console renders it, and judging SVG shape here would fail accounts for a
	// reason the operator never asked for.
	return service.OAuthTestAnswer{
		Question1:     answer1,
		Question2HTML: normalizeOAuthTestHTML(answer2),
	}, nil
}

// oauthTestRetryWait is how long a probe waits before asking a busy account
// again. A variable so tests do not sleep for real.
var oauthTestRetryWait = 15 * time.Second

// runOAuthTestPrompt asks one question of the pinned account and returns its text.
//
// The host model call is not context-aware, so the question budget is enforced
// here: the caller stops waiting and the abandoned attempt finishes in the
// background with its lease closed either way.
func runOAuthTestPrompt(ctx context.Context, request service.OAuthTestRequest, prompt string) (string, error) {
	type callResult struct {
		text string
		err  error
	}
	results := make(chan callResult, 1)
	go func() {
		text, err := askPinnedAccount(ctx, request, prompt)
		results <- callResult{text: text, err: err}
	}()

	timer := time.NewTimer(service.OAuthTestQuestionTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("调用被取消: %w", ctx.Err())
	case <-timer.C:
		return "", fmt.Errorf("超过 %s 未返回", service.OAuthTestQuestionTimeout)
	case result := <-results:
		return result.text, result.err
	}
}

// askPinnedAccount runs the retry loop for one question. An account that already
// serves its concurrency cap refuses the pick instead of queueing, so the probe
// waits and asks again until the question budget runs out; every other failure is
// returned as it is, because the sweep records it and moves on to the next account.
//
// Each attempt uses a fresh nonce: the lease behind the pin is finished after
// every attempt, and an expired lease would silently turn the next attempt into
// ordinary scheduling.
func askPinnedAccount(ctx context.Context, request service.OAuthTestRequest, prompt string) (string, error) {
	payload := map[string]any{
		"model":    request.Model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	}
	// Chat Completions carries thinking intensity in reasoning_effort. The
	// Responses-only "reasoning" object is deliberately not sent: a strict
	// upstream rejects unknown parameters, and the probe must not fail on shape.
	if intensity := strings.TrimSpace(request.ThinkingIntensity); intensity != "" {
		payload["reasoning_effort"] = intensity
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("调用被取消: %w", err)
		}
		nonce, err := newWarmupNonce()
		if err != nil {
			return "", err
		}
		now := time.Now().UTC()
		registerWarmupLease(&warmupLease{
			RunID:     request.RunID,
			Nonce:     nonce,
			Auth:      request.Auth,
			Model:     request.Model,
			StartedAt: now,
			ExpiresAt: now.Add(service.OAuthTestQuestionTimeout),
		})
		headers := make(http.Header)
		headers.Set(authWarmupHeader, nonce)
		response, _, status, callErr := hostModelExecute("", pluginapi.ExecutorRequest{
			SourceFormat: "openai",
			Format:       "openai",
			Model:        request.Model,
			Headers:      headers,
		}, body, false)
		finishWarmupLease(nonce, time.Now().UTC(), serviceTokenUsage(response))

		if callErr == nil && status >= 200 && status < 300 {
			text := extractOAuthTestText(response)
			if strings.TrimSpace(text) == "" {
				return "", errors.New("模型返回为空")
			}
			return text, nil
		}
		rejection := callErr
		if rejection == nil {
			rejection = fmt.Errorf("上游返回 HTTP %d: %s", status, truncateText(string(response), 200))
		}
		if !isOAuthTestBusy(rejection, status) {
			return "", rejection
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("等待账号空闲时被取消: %w", ctx.Err())
		case <-time.After(oauthTestRetryWait):
		}
	}
}

// isOAuthTestBusy reports whether the account refused because it is already at
// its concurrency cap. Those are the only rejections worth waiting out.
func isOAuthTestBusy(err error, status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"warmup target unavailable",
		"currently unavailable",
		"concurrency",
		"at capacity",
		"rate limit",
		"too many requests",
		"429",
		"并发",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// serviceTokenUsage reads the reported usage so the warmup lease records what the
// probe actually spent upstream instead of a hardcoded zero.
func serviceTokenUsage(raw []byte) money.TokenUsage {
	if len(raw) == 0 {
		return money.TokenUsage{}
	}
	parsed := usageparse.FromResponseBody(raw, "openai")
	if !parsed.Found {
		return money.TokenUsage{}
	}
	return parsed.Usage
}

// extractOAuthTestText pulls the assistant text out of a non-streaming response.
// Every failure path returns an empty string: the caller reports "empty answer"
// rather than showing the operator a JSON envelope as if it were an answer.
func extractOAuthTestText(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(trimmed), &payload) == nil {
		if text := chatCompletionText(payload); text != "" {
			return text
		}
		if text, ok := payload["output_text"].(string); ok {
			return strings.TrimSpace(text)
		}
		if text := responsesOutputText(payload); text != "" {
			return text
		}
		return ""
	}
	return sseAssistantText(trimmed)
}

func chatCompletionText(payload map[string]any) string {
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	if message, ok := choice["message"].(map[string]any); ok {
		if content, ok := message["content"].(string); ok {
			return strings.TrimSpace(content)
		}
	}
	if text, ok := choice["text"].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// responsesOutputText walks the Responses API shape, whose assistant text lives
// in output[].content[].text.
func responsesOutputText(payload map[string]any) string {
	output, ok := payload["output"].([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, rawItem := range output {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		contents, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawContent := range contents {
			content, ok := rawContent.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := content["text"].(string); ok {
				builder.WriteString(text)
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

// sseAssistantText reads the concatenated deltas of a stream that arrived even
// though the probe asked for a non-streaming response.
func sseAssistantText(raw string) string {
	var builder strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(data), &payload) != nil {
			continue
		}
		choices, ok := payload["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if content, ok := delta["content"].(string); ok {
				builder.WriteString(content)
			}
		}
	}
	return strings.TrimSpace(builder.String())
}

// htmlFence strips a Markdown code fence from an HTML answer.
var htmlFence = regexp.MustCompile("(?is)^```(?:html)?\\s*(.*?)\\s*```$")

func normalizeOAuthTestHTML(raw string) string {
	raw = strings.TrimSpace(raw)
	if match := htmlFence.FindStringSubmatch(raw); len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return raw
}
