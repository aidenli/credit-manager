package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestParseAuthWeightMirrorsHost(t *testing.T) {
	cases := map[string]int64{
		"":          DefaultAuthWeight,
		"  ":        DefaultAuthWeight,
		"1":         1,
		" 3 ":       3,
		"1000000":   MaxAuthWeight,
		"1000001":   -1,
		"0":         -1,
		"-2":        -1,
		"1.5":       -1,
		"heavy":     -1,
		"999999999": -1,
	}
	for raw, want := range cases {
		if got := ParseAuthWeight(raw); got != want {
			t.Errorf("ParseAuthWeight(%q) = %d, want %d", raw, got, want)
		}
	}
	// A zero-value candidate (no weight forwarded) keeps the default weight.
	if got := effectiveAuthWeight(AuthPickCandidate{}); got != DefaultAuthWeight {
		t.Fatalf("unset weight = %d, want default", got)
	}
}

func weightedCandidates(weights ...int64) []AuthPickCandidate {
	out := make([]AuthPickCandidate, 0, len(weights))
	for i, weight := range weights {
		out = append(out, AuthPickCandidate{ID: fmt.Sprintf("account-%d", i+1), Provider: "codex", Priority: 2, Weight: weight})
	}
	return out
}

func TestWeightedPickFollowsRatio(t *testing.T) {
	s := &Service{}
	candidates := weightedCandidates(3, 1)
	counts := map[string]int{}
	for range 400 {
		counts[s.nextWeightedAuthPickLocked("scope", candidates).ID]++
	}
	if counts["account-1"] != 300 || counts["account-2"] != 100 {
		t.Fatalf("3:1 weights distributed %v", counts)
	}
}

func TestWeightedPickEqualWeightsAlternate(t *testing.T) {
	s := &Service{}
	// Unset (0) and explicit 1 are the same default weight.
	candidates := weightedCandidates(0, 1)
	for i := range 6 {
		want := candidates[i%2].ID
		if got := s.nextWeightedAuthPickLocked("scope", candidates).ID; got != want {
			t.Fatalf("pick %d = %q, want %q", i, got, want)
		}
	}
}

// A temporarily shrunken candidate set must keep accumulated credits, otherwise
// selection would collapse onto the first candidate after every exclusion.
func TestWeightedPickKeepsCreditsWhenCandidatesShrink(t *testing.T) {
	s := &Service{}
	full := weightedCandidates(1, 1)
	if got := s.nextWeightedAuthPickLocked("scope", full).ID; got != "account-1" {
		t.Fatalf("first pick = %q", got)
	}
	// account-2 is briefly the only option (account-1 at capacity).
	if got := s.nextWeightedAuthPickLocked("scope", full[1:]).ID; got != "account-2" {
		t.Fatalf("shrunken pick = %q", got)
	}
	counts := map[string]int{}
	for range 10 {
		counts[s.nextWeightedAuthPickLocked("scope", full).ID]++
	}
	if counts["account-1"] != 5 || counts["account-2"] != 5 {
		t.Fatalf("credits lost after shrink: %v", counts)
	}
}

func TestWeightedPickResetsWhenWeightChanges(t *testing.T) {
	s := &Service{}
	for range 7 {
		s.nextWeightedAuthPickLocked("scope", weightedCandidates(1, 1))
	}
	changed := weightedCandidates(1, 3)
	counts := map[string]int{}
	for range 40 {
		counts[s.nextWeightedAuthPickLocked("scope", changed).ID]++
	}
	if counts["account-1"] != 10 || counts["account-2"] != 30 {
		t.Fatalf("new 1:3 weights distributed %v", counts)
	}
}

func TestWeightedPickScopesAreIndependent(t *testing.T) {
	s := &Service{}
	candidates := weightedCandidates(1, 1)
	a := s.nextWeightedAuthPickLocked("key-a", candidates).ID
	b := s.nextWeightedAuthPickLocked("key-b", candidates).ID
	if a != "account-1" || b != "account-1" {
		t.Fatalf("scopes share state: %q %q", a, b)
	}
}

func TestPositiveWeightAuthsDropsExcluded(t *testing.T) {
	got := positiveWeightAuths(weightedCandidates(-1, 0, 2))
	if len(got) != 2 || got[0].ID != "account-2" || got[1].ID != "account-3" {
		t.Fatalf("positiveWeightAuths = %#v", got)
	}
}

func weightedBoundPick(t *testing.T, s *Service, headers map[string][]string, candidates []AuthPickCandidate) (string, error) {
	t.Helper()
	id, handled, err := s.PickAuthForKey(context.Background(), headers, candidates, "gpt-5")
	if !handled {
		t.Fatalf("pick not handled: (%q, %v)", id, err)
	}
	return id, err
}

// New sessions are spread by weight; an existing session stays on its account.
func TestSessionAffinityNewSessionsFollowWeights(t *testing.T) {
	s, key, headers := affinityService(t, true)
	bindTwo(t, s, key.ID)
	candidates := []AuthPickCandidate{
		{ID: "account-1", Provider: "codex", Priority: 2, Weight: 3},
		{ID: "account-2", Provider: "codex", Priority: 2, Weight: 1},
	}
	counts := map[string]int{}
	for i := range 40 {
		id, err := weightedBoundPick(t, s, sessionHeaders(headers, fmt.Sprintf("session-%d", i)), candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[id]++
	}
	if counts["account-1"] != 30 || counts["account-2"] != 10 {
		t.Fatalf("new sessions distributed %v, want 30:10", counts)
	}
	// Sticky sessions do not consume weighted turns or drift.
	pinned, err := weightedBoundPick(t, s, sessionHeaders(headers, "session-1"), candidates)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if got, _ := weightedBoundPick(t, s, sessionHeaders(headers, "session-1"), candidates); got != pinned {
			t.Fatalf("session drifted %q -> %q", pinned, got)
		}
	}
}

// Setting weight 0 on the pinned account moves the session to a sibling.
func TestSessionAffinityLeavesExcludedWeight(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	session := sessionHeaders(headers, "session-a")
	first, err := weightedBoundPick(t, s, session, candidates)
	if err != nil {
		t.Fatal(err)
	}
	excluded := make([]AuthPickCandidate, len(candidates))
	copy(excluded, candidates)
	for i := range excluded {
		if excluded[i].ID == first {
			excluded[i].Weight = ParseAuthWeight("0")
		}
	}
	for range 3 {
		got, err := weightedBoundPick(t, s, session, excluded)
		if err != nil {
			t.Fatal(err)
		}
		if got == first {
			t.Fatalf("weight-0 account %q still selected", first)
		}
	}
}

// Weight never lets a lower-priority credential beat the highest tier, and a
// highest tier whose weights all exclude it fails closed instead of demoting.
func TestWeightDoesNotCrossPriorityTiers(t *testing.T) {
	s, key, headers := affinityService(t, false)
	bindTwo(t, s, key.ID)
	candidates := []AuthPickCandidate{
		{ID: "account-1", Provider: "codex", Priority: 2, Weight: 1},
		{ID: "account-2", Provider: "codex", Priority: 1, Weight: 1000},
	}
	for range 5 {
		if got, err := weightedBoundPick(t, s, headers, candidates); err != nil || got != "account-1" {
			t.Fatalf("pick = (%q, %v), want account-1", got, err)
		}
	}
	candidates[0].Weight = -1
	if got, err := weightedBoundPick(t, s, headers, candidates); !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("all-excluded top tier = (%q, %v), want ErrNoBoundAuthAvailable", got, err)
	}
}

func TestUnboundKeyAllExcludedWeightsFailClosed(t *testing.T) {
	s, _, headers := affinityService(t, false)
	candidates := []AuthPickCandidate{{ID: "account-1", Provider: "codex", Priority: 2, Weight: -1}}
	if got, err := weightedBoundPick(t, s, headers, candidates); !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("unbound all-excluded = (%q, %v), want ErrConcurrentLimit", got, err)
	}
}
