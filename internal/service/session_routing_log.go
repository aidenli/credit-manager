package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

const maxRoutingLogCandidates = 64

// SessionRoutingDecision is a bounded, body-free snapshot of a successful pick.
// NewBinding means a cache miss in this routing epoch, not a new client chat.
// Session/key hashes use an ephemeral private HMAC key; account hashes are stable
// SHA-256 pseudonyms so operators can map known accounts without logging their IDs.
type SessionRoutingDecision struct {
	Epoch               string               `json:"routing_epoch"`
	Sequence            uint64               `json:"sequence"`
	At                  string               `json:"at"`
	Reason              string               `json:"reason"`
	SessionHash         string               `json:"session_hash,omitempty"`
	SessionSource       string               `json:"session_source"`
	HeaderSessionHash   string               `json:"header_session_hash,omitempty"`
	HeaderSessionSource string               `json:"header_session_source"`
	BindingHash         string               `json:"binding_hash,omitempty"`
	KeyHash             string               `json:"key_hash"`
	Model               string               `json:"model"`
	AffinityEnabled     bool                 `json:"affinity_enabled"`
	TTLSeconds          int64                `json:"ttl_seconds"`
	SelectionScope      string               `json:"selection_scope"`
	ChosenAuthHash      string               `json:"chosen_auth_hash"`
	ChosenProvider      string               `json:"chosen_provider"`
	ChosenWeight        int64                `json:"chosen_weight"`
	CandidateCount      int                  `json:"candidate_count"`
	CandidatesTruncated bool                 `json:"candidates_truncated"`
	Candidates          []SessionRoutingAuth `json:"candidates"`
}

type SessionRoutingAuth struct {
	AuthHash string `json:"auth_hash"`
	Provider string `json:"provider"`
	Priority int    `json:"priority"`
	Weight   int64  `json:"weight"`
}

type sessionRoutingMonitor struct {
	secret   [32]byte
	epoch    string
	sequence uint64
}

func (m *sessionRoutingMonitor) hash(kind, value string) string {
	if value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, m.secret[:])
	_, _ = mac.Write([]byte(kind + "\x00" + value))
	return hex.EncodeToString(mac.Sum(nil))
}

// captureSessionRoutingDecisionLocked performs no log/DB IO and stores no per-session
// telemetry state. The first monitored pick obtains OS entropy once for this epoch.
// The caller emits the returned snapshot only after releasing authMu.
func (s *Service) captureSessionRoutingDecisionLocked(out *SessionRoutingDecision, ctx context.Context, headers http.Header, keyID, model, reason string, chosen AuthPickCandidate, candidates []AuthPickCandidate, now time.Time) {
	if s.routingMonitor == nil {
		m := &sessionRoutingMonitor{}
		if _, err := rand.Read(m.secret[:]); err != nil {
			return // Monitoring must never prevent routing.
		}
		m.epoch = m.hash("epoch", "credit-manager")
		s.routingMonitor = m
	}
	m := s.routingMonitor
	m.sequence++
	sessionID, source := requestSessionIdentity(ctx, headers)
	headerID, headerSource := headerSessionIdentity(headers)
	settings := s.sessionAffinitySettings()
	scope := "per_key"
	bindingHash := ""
	if settings.Enabled && sessionID != "" {
		scope = "shared_session"
		bindingHash = m.hash("binding", sessionAffinityKey("mixed", sessionID, strings.TrimSpace(model)))
	}
	*out = SessionRoutingDecision{
		Epoch: m.epoch, Sequence: m.sequence, At: now.UTC().Format(time.RFC3339Nano), Reason: reason,
		SessionHash: m.hash("session", sessionID), SessionSource: source,
		HeaderSessionHash: m.hash("session", headerID), HeaderSessionSource: headerSource,
		BindingHash: bindingHash, KeyHash: m.hash("key", keyID), Model: routingLogText(model),
		AffinityEnabled: settings.Enabled, TTLSeconds: int64(settings.TTL / time.Second), SelectionScope: scope,
		ChosenAuthHash: routingAuthHash(chosen), ChosenProvider: routingLogText(chosen.Provider), ChosenWeight: effectiveAuthWeight(chosen),
		CandidateCount: len(candidates), CandidatesTruncated: len(candidates) > maxRoutingLogCandidates,
	}
	for i, candidate := range candidates {
		if i >= maxRoutingLogCandidates {
			break
		}
		out.Candidates = append(out.Candidates, SessionRoutingAuth{
			AuthHash: routingAuthHash(candidate), Provider: routingLogText(candidate.Provider), Priority: candidate.Priority, Weight: effectiveAuthWeight(candidate),
		})
	}
}

func routingAuthHash(candidate AuthPickCandidate) string {
	sum := sha256.Sum256([]byte(weightedPickID(candidate)))
	return hex.EncodeToString(sum[:])
}

func routingLogText(value string) string {
	if len(value) > 128 {
		value = value[:128]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
}
