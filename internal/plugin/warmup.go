package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
	"github.com/yuluo688/credit-manager/internal/usageparse"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const authWarmupHeader = "X-Credit-Manager-Warmup"

type warmupLease struct {
	RunID       string
	Nonce       string
	Auth        store.AuthIdentity
	Model       string
	StartedAt   time.Time
	CompletedAt time.Time
	ExpiresAt   time.Time
	Usage       money.TokenUsage
}

var activeWarmups = struct {
	sync.Mutex
	byNonce map[string]*warmupLease
}{byNonce: make(map[string]*warmupLease)}

type hostAuthWarmupExecutor struct{}

func (hostAuthWarmupExecutor) ExecuteAuthWarmup(ctx context.Context, request service.AuthWarmupRequest) (service.AuthWarmupResult, error) {
	if request.Auth.Empty() || len(request.Models) == 0 {
		return service.AuthWarmupResult{}, errors.New("warmup target unavailable")
	}
	startedAt := request.RequestedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	body, err := json.Marshal(map[string]any{
		"messages":   []map[string]string{{"role": "user", "content": "."}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return service.AuthWarmupResult{}, err
	}
	for _, model := range request.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		nonce, err := newWarmupNonce()
		if err != nil {
			return service.AuthWarmupResult{}, err
		}
		lease := &warmupLease{RunID: request.RunID, Nonce: nonce, Auth: request.Auth, Model: model, StartedAt: startedAt, ExpiresAt: startedAt.Add(2 * time.Minute)}
		registerWarmupLease(lease)
		headers := make(http.Header)
		headers.Set(authWarmupHeader, nonce)
		response, _, status, err := hostModelExecute("", pluginapi.ExecutorRequest{
			SourceFormat: firstNonEmpty(request.EntryProtocol, "openai"),
			Format:       firstNonEmpty(request.EntryProtocol, "openai"),
			Model:        model,
			Headers:      headers,
		}, body, false)
		if err != nil && isWarmupTargetUnavailable(err) {
			finishWarmupLease(nonce, time.Now().UTC(), money.TokenUsage{})
			continue
		}
		if err != nil {
			finishWarmupLease(nonce, time.Now().UTC(), money.TokenUsage{})
			return service.AuthWarmupResult{}, err
		}
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			finishWarmupLease(nonce, time.Now().UTC(), money.TokenUsage{})
			return service.AuthWarmupResult{}, warmupResponseError(status, response)
		}
		parsed := usageparse.FromResponseBody(response, request.EntryProtocol)
		finishWarmupLease(nonce, time.Now().UTC(), parsed.Usage)
		return service.AuthWarmupResult{Usage: parsed.Usage, Model: model}, nil
	}
	return service.AuthWarmupResult{}, service.ErrAuthWarmupModelUnavailable
}

// warmupResponseError retains only safe, actionable upstream classifications.
// Never persist opaque provider bodies because they can contain request context.
func warmupResponseError(status int, body []byte) error {
	message := strings.ToLower(string(body))
	switch {
	case strings.Contains(message, "missing api key"):
		return errors.New("warmup request missing API key")
	case strings.Contains(message, "unauthorized"), status == http.StatusUnauthorized:
		return errors.New("warmup request unauthorized")
	case strings.Contains(message, "rate limit"), status == http.StatusTooManyRequests:
		return errors.New("warmup request rate limited")
	default:
		return fmt.Errorf("warmup request returned status %d", status)
	}
}

func isWarmupTargetUnavailable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "warmup target unavailable")
}

func newWarmupNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate warmup nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func registerWarmupLease(lease *warmupLease) {
	if lease == nil || strings.TrimSpace(lease.Nonce) == "" {
		return
	}
	activeWarmups.Lock()
	defer activeWarmups.Unlock()
	pruneWarmupLeasesLocked(time.Now())
	activeWarmups.byNonce[lease.Nonce] = lease
}

func finishWarmupLease(nonce string, completedAt time.Time, usage money.TokenUsage) {
	activeWarmups.Lock()
	defer activeWarmups.Unlock()
	if lease := activeWarmups.byNonce[strings.TrimSpace(nonce)]; lease != nil {
		lease.CompletedAt = completedAt.UTC()
		lease.ExpiresAt = completedAt.Add(2 * time.Minute)
		lease.Usage = usage
	}
}

func warmupTarget(headers http.Header, candidates []service.AuthPickCandidate) (string, bool, error) {
	nonce := strings.TrimSpace(headers.Get(authWarmupHeader))
	if nonce == "" {
		return "", false, nil
	}
	activeWarmups.Lock()
	defer activeWarmups.Unlock()
	pruneWarmupLeasesLocked(time.Now())
	lease := activeWarmups.byNonce[nonce]
	if lease == nil {
		// Unknown headers must never influence normal client scheduling.
		return "", false, nil
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == strings.TrimSpace(lease.Auth.AuthID) && warmupProvidersMatch(candidate.Provider, lease.Auth.Provider) {
			return candidate.ID, true, nil
		}
	}
	return "", true, errors.New("warmup target unavailable")
}

func isWarmupUsage(record pluginapi.UsageRecord) bool {
	activeWarmups.Lock()
	defer activeWarmups.Unlock()
	pruneWarmupLeasesLocked(time.Now())
	for _, lease := range activeWarmups.byNonce {
		matchesAuth := lease != nil && (strings.TrimSpace(record.AuthID) == strings.TrimSpace(lease.Auth.AuthID) ||
			(strings.TrimSpace(lease.Auth.AuthIndex) != "" && strings.TrimSpace(record.AuthIndex) == strings.TrimSpace(lease.Auth.AuthIndex)))
		matchesModel := lease != nil && (strings.TrimSpace(record.Model) == strings.TrimSpace(lease.Model) || strings.TrimSpace(record.Alias) == strings.TrimSpace(lease.Model))
		if !matchesAuth || !warmupProvidersMatch(record.Provider, lease.Auth.Provider) || !matchesModel {
			continue
		}
		if !lease.Usage.HasTokens() || record.Detail.InputTokens != lease.Usage.Input || record.Detail.OutputTokens != lease.Usage.Output {
			continue
		}
		requestedAt := record.RequestedAt
		if requestedAt.IsZero() {
			continue
		}
		end := lease.CompletedAt
		if end.IsZero() {
			end = time.Now()
		}
		if !requestedAt.Before(lease.StartedAt.Add(-time.Second)) && !requestedAt.After(end.Add(time.Second)) {
			return true
		}
	}
	return false
}

func pruneWarmupLeasesLocked(now time.Time) {
	for nonce, lease := range activeWarmups.byNonce {
		if lease == nil || !lease.ExpiresAt.After(now) {
			delete(activeWarmups.byNonce, nonce)
		}
	}
}

func warmupProvidersMatch(left, right string) bool {
	normalize := func(value string) string {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "openai", "chatgpt", "codex":
			return "codex"
		case "anthropic", "claude":
			return "claude"
		case "google", "gemini", "antigravity":
			return "antigravity"
		case "moonshot", "kimi":
			return "kimi"
		case "grok", "xai":
			return "xai"
		default:
			return strings.ToLower(strings.TrimSpace(value))
		}
	}
	return normalize(left) != "" && normalize(left) == normalize(right)
}
