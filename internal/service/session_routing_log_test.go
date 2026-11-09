package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSessionRoutingLogWeightedBindingsAndStickyHits(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	candidates[0].Weight, candidates[1].Weight = 1, 5
	ctx := context.Background()
	if _, _, decision, err := s.PickAuthForKeyWithDecision(ctx, sessionHeaders(headers, "off"), candidates, "gpt-5"); err != nil || decision != nil || s.routingMonitor != nil {
		t.Fatalf("disabled monitoring produced state/log: %+v, %v", decision, err)
	}
	// Start a fresh cursor so the measured 60 picks cover ten complete cycles.
	s.authWeightedPick = nil
	s.cfg.SessionAffinity.LogDecisions = true
	counts := map[string]int{}
	var previous *SessionRoutingDecision
	for i := 0; i < 60; i++ {
		h := sessionHeaders(headers, fmt.Sprintf("private-session-%d", i))
		id, handled, d, err := s.PickAuthForKeyWithDecision(ctx, h, candidates, "gpt-5")
		if err != nil || !handled || d == nil || d.Reason != "new_binding" || d.CandidateCount != 2 || d.ChosenAuthHash == "" || d.SelectionScope != "shared_session" {
			t.Fatalf("new binding: %s %+v %v", id, d, err)
		}
		counts[id]++
		if previous != nil && (previous.Epoch != d.Epoch || previous.Sequence >= d.Sequence || previous.SessionHash == d.SessionHash) {
			t.Fatalf("unstable epoch/sequence/session identity: %+v / %+v", previous, d)
		}
		_, _, hit, err := s.PickAuthForKeyWithDecision(ctx, h, candidates, "gpt-5")
		if err != nil || hit.Reason != "affinity_hit" || hit.SessionHash != d.SessionHash || hit.BindingHash != d.BindingHash || hit.ChosenAuthHash != d.ChosenAuthHash {
			t.Fatalf("sticky hit changed routing: %+v %v", hit, err)
		}
		encoded, _ := json.Marshal(d)
		for _, secret := range []string{"private-session-", key.ID, headers.Get("Authorization"), "account-1", "account-2"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("raw identity leaked: %s", encoded)
			}
		}
		previous = d
	}
	if counts["account-1"] != 10 || counts["account-2"] != 50 {
		t.Fatalf("expected new-binding ratio 1:5, got %v", counts)
	}
}

func TestSessionRoutingLogReasonsAndHostIdentity(t *testing.T) {
	s, key, headers := affinityService(t, true)
	s.cfg.SessionAffinity.LogDecisions = true
	candidates := bindTwo(t, s, key.ID)
	ctx := WithSchedulerMetadata(context.Background(), map[string]any{hostCanonicalSessionMetadataKey: "canonical-private"})
	pick := func(ctx context.Context, h http.Header, pool []AuthPickCandidate) *SessionRoutingDecision {
		t.Helper()
		_, _, d, err := s.PickAuthForKeyWithDecision(ctx, h, pool, "gpt-5")
		if err != nil || d == nil {
			t.Fatalf("pick failed: %+v %v", d, err)
		}
		return d
	}
	first := pick(ctx, sessionHeaders(headers, "different-header-1"), candidates)
	hit := pick(ctx, sessionHeaders(headers, "different-header-2"), candidates)
	if hit.Reason != "affinity_hit" || hit.SessionSource != "host_canonical" || first.SessionHash != hit.SessionHash || first.HeaderSessionHash == hit.HeaderSessionHash || hit.HeaderSessionSource != "Session-Id" {
		t.Fatalf("host-overridden distinct headers not visible: %+v %+v", first, hit)
	}
	bindingKey := sessionAffinityKey("mixed", "host:canonical-private", "gpt-5")
	binding := s.sessionAffinity[bindingKey]
	binding.expires = time.Now().Add(-time.Second)
	s.sessionAffinity[bindingKey] = binding
	if d := pick(ctx, headers, candidates); d.Reason != "expired_rebind" {
		t.Fatalf("expected expired_rebind: %+v", d)
	}
	binding = s.sessionAffinity[bindingKey]
	var other []AuthPickCandidate
	for _, c := range candidates {
		if c.ID != binding.authID {
			other = append(other, c)
		}
	}
	if d := pick(ctx, headers, other); d.Reason != "unavailable_rebind" || d.CandidateCount != 1 {
		t.Fatalf("expected unavailable_rebind: %+v", d)
	}
	if d := pick(context.Background(), headers, candidates); d.Reason != "no_session" || d.SessionSource != "none" {
		t.Fatalf("expected no_session: %+v", d)
	}
	s.setSessionAffinityRuntime(false, time.Hour)
	if d := pick(ctx, headers, candidates); d.Reason != "affinity_disabled" || d.SelectionScope != "per_key" {
		t.Fatalf("expected affinity_disabled: %+v", d)
	}
}

func TestSessionRoutingLogSnapshotBounded(t *testing.T) {
	s := &Service{}
	s.setSessionAffinityRuntime(true, time.Hour)
	candidates := make([]AuthPickCandidate, 100)
	for i := range candidates {
		candidates[i] = AuthPickCandidate{ID: fmt.Sprint(i), Provider: "codex", Weight: 1}
	}
	var d SessionRoutingDecision
	s.captureSessionRoutingDecisionLocked(&d, context.Background(), nil, "key", strings.Repeat("x", 10000)+"\n", "no_session", candidates[0], candidates, time.Now())
	if len(d.Model) != 128 || len(d.Candidates) != maxRoutingLogCandidates || !d.CandidatesTruncated || d.CandidateCount != 100 {
		t.Fatalf("unbounded snapshot: %+v", d)
	}
	oldHash := d.ChosenAuthHash
	oldEpoch := d.Epoch
	s.routingMonitor = nil
	s.captureSessionRoutingDecisionLocked(&d, context.Background(), nil, "key", "model", "no_session", candidates[0], candidates, time.Now())
	if d.Epoch == oldEpoch || d.ChosenAuthHash != oldHash {
		t.Fatal("account pseudonyms must survive epoch changes; session hashes must not")
	}
}
