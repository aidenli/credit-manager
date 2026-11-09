package service

import (
	"context"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

// Permission and availability are evaluated before priority, including when the
// host offers other keys' OAuth credentials alongside lower-priority API ones.
func TestBoundSchedulerUsesHighestAuthorizedPriority(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []AuthPickCandidate
		want       string
	}{
		{"missing bound account does not expose another OAuth", []AuthPickCandidate{
			{ID: "other-oauth", Provider: "codex", Priority: 2},
			{ID: apiAccount1, Provider: apiProvider, Priority: 1},
		}, apiAccount1},
		{"API priority beats bound OAuth", []AuthPickCandidate{
			{ID: "bound", Provider: "codex", Priority: 1},
			{ID: apiAccount1, Provider: apiProvider, Priority: 2},
		}, apiAccount1},
		{"bound OAuth priority beats API", []AuthPickCandidate{
			{ID: apiAccount1, Provider: apiProvider, Priority: 1},
			{ID: "bound", Provider: "codex", Priority: 2},
		}, "bound"},
		{"negative priority is not clamped to zero", []AuthPickCandidate{
			{ID: "bound", Provider: "codex", Priority: -3},
			{ID: apiAccount1, Provider: apiProvider, Priority: -2},
		}, apiAccount1},
		{"advisory error does not demote the highest tier", []AuthPickCandidate{
			{ID: "bound", Provider: "codex", Priority: 2, Status: "error"},
			{ID: apiAccount1, Provider: apiProvider, Priority: 1, Status: "active"},
		}, "bound"},
		{"disabled highest credential is ineligible", []AuthPickCandidate{
			{ID: "bound", Provider: "codex", Priority: 2, Status: "disabled"},
			{ID: apiAccount1, Provider: apiProvider, Priority: 1},
		}, apiAccount1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fallbackService(t, false)
			// These options belong to the legacy execution replay, not scheduling.
			s.cfg.APIFallback.Providers = []string{"openai-compatible-not-offered"}
			key, headers := boundKey(t, s, "priority")
			bindOneAccount(t, s, key.ID, "bound")
			id, handled, err := s.PickAuthForKey(context.Background(), headers, tc.candidates, "model")
			if err != nil || !handled || id != tc.want {
				t.Fatalf("priority pick = (%q, %v, %v), want %q", id, handled, err, tc.want)
			}
		})
	}
}

func TestBoundSchedulerAvailabilityPrecedesPriority(t *testing.T) {
	for _, hold := range []string{"concurrency", "warmup"} {
		t.Run(hold, func(t *testing.T) {
			s := fallbackService(t, false)
			s.cfg.SessionAffinity.LogDecisions = true
			ctx := context.Background()
			key, headers := boundKey(t, s, "available-first")
			bindOneAccount(t, s, key.ID, "high")
			candidates := []AuthPickCandidate{
				{ID: "high", Provider: "codex", Priority: 2, Status: "error"},
				{ID: apiAccount1, Provider: apiProvider, Priority: 1},
			}
			identity := store.AuthIdentity{AuthID: "high", Provider: "codex"}
			if hold == "concurrency" {
				if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "high", 1); err != nil {
					t.Fatal(err)
				}
				s.TrackAuthCapture("occupied", "model")
				if err := s.AdmitAuth(ctx, "occupied", identity); err != nil {
					t.Fatal(err)
				}
			} else if err := s.AcquireAuthWarmup(ctx, "warmup", identity); err != nil {
				t.Fatal(err)
			}
			id, handled, decision, err := s.PickAuthForKeyWithDecision(ctx, headers, candidates, "model")
			if err != nil || !handled || id != apiAccount1 {
				t.Fatalf("held highest tier must not hide available API = (%q, %v, %v)", id, handled, err)
			}
			if decision == nil || decision.HostCandidateCount != 2 || decision.ExcludedBusyCount != 1 || decision.CandidateCount != 1 {
				t.Fatalf("availability exclusion diagnostics: %+v", decision)
			}
			if hold == "concurrency" {
				s.FinishAuthCapture("occupied")
			} else {
				s.ReleaseAuthWarmup("warmup")
			}
			id, handled, err = s.PickAuthForKey(ctx, headers, candidates, "model")
			if err != nil || !handled || id != "high" {
				t.Fatalf("released highest tier = (%q, %v, %v)", id, handled, err)
			}
		})
	}
}

func TestSchedulerAlwaysHonoursPriorityWithoutLegacyProviderAllowlist(t *testing.T) {
	s := fallbackService(t, true)
	key, headers := boundKey(t, s, "priority-without-allowlist")
	bindOneAccount(t, s, key.ID, "missing")
	candidates := []AuthPickCandidate{
		{ID: "low", Provider: apiProvider, Priority: -1},
		{ID: "high", Provider: apiProvider, Priority: 10},
	}
	for range 4 {
		id, handled, err := s.PickAuthForKey(context.Background(), headers, candidates, "model")
		if err != nil || !handled || id != "high" {
			t.Fatalf("highest-priority pick = (%q, %v, %v)", id, handled, err)
		}
	}
}

func TestSchedulerRotatesCredentialsNotProviderGroups(t *testing.T) {
	s := fallbackService(t, false)
	// Excluding both providers from this old execution option must not matter.
	s.cfg.APIFallback.Providers = []string{"openai-compatible-not-offered"}
	key, headers := boundKey(t, s, "credential-rotation")
	bindOneAccount(t, s, key.ID, "missing")
	candidates := []AuthPickCandidate{
		{ID: "a1", Provider: apiProvider, Priority: 2},
		{ID: "a2", Provider: apiProvider, Priority: 2},
		{ID: "a3", Provider: apiProvider, Priority: 2},
		{ID: "b1", Provider: "openai-compatible-other", Priority: 2},
	}
	seen := map[string]int{}
	for range 4 {
		id, handled, err := s.PickAuthForKey(context.Background(), headers, candidates, "model")
		if err != nil || !handled {
			t.Fatalf("pick = (%q, %v, %v)", id, handled, err)
		}
		seen[id]++
	}
	for _, candidate := range candidates {
		if seen[candidate.ID] != 1 {
			t.Fatalf("credential rotation = %v", seen)
		}
	}
}

func TestUnboundPluginSchedulerHonoursPriority(t *testing.T) {
	for _, hold := range []string{"none", "concurrency", "warmup"} {
		t.Run(hold, func(t *testing.T) {
			s := quotaService(t)
			ctx := context.Background()
			_, headers := boundKey(t, s, "unbound-priority")
			candidates := []AuthPickCandidate{
				{ID: "low", Provider: "codex", Priority: -2},
				{ID: "high", Provider: apiProvider, Priority: -1},
			}
			if hold == "concurrency" {
				if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "low", 1); err != nil {
					t.Fatal(err)
				}
			} else if hold == "warmup" {
				if err := s.AcquireAuthWarmup(ctx, "warmup", store.AuthIdentity{AuthID: "low", Provider: "codex"}); err != nil {
					t.Fatal(err)
				}
			}
			id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "model")
			if err != nil || !handled || id != "high" {
				t.Fatalf("global priority pick = (%q, %v, %v)", id, handled, err)
			}
		})
	}
}
