package service

import (
	"context"
	"errors"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestFallbackAPIPriorityAfterAvailability(t *testing.T) {
	s := fallbackService(t, true)
	s.cfg.APIFallback.Providers = []string{apiProvider}
	ctx := context.Background()
	key, headers := boundKey(t, s, "priority")
	bindOneAccount(t, s, key.ID, "missing-bound")
	candidates := []AuthPickCandidate{
		{ID: "oauth-other", Provider: "codex", Priority: 100},
		{ID: "low", Provider: apiProvider, Priority: -2},
		{ID: "high", Provider: apiProvider, Priority: -1},
	}
	id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "model")
	if err != nil || !handled || id != "high" {
		t.Fatalf("priority pick = (%q, %v, %v)", id, handled, err)
	}
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, apiProvider, "high", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("occupied", "model")
	if err := s.AdmitAuth(ctx, "occupied", store.AuthIdentity{AuthID: "high", Provider: apiProvider}); err != nil {
		t.Fatal(err)
	}
	id, handled, err = s.PickAuthForKey(ctx, headers, candidates, "model")
	if err != nil || !handled || id != "low" {
		t.Fatalf("busy high tier must not hide lower available tier = (%q, %v, %v)", id, handled, err)
	}
}

func TestLegacyFallbackKeepsCredentialRotationAcrossPriorities(t *testing.T) {
	s := fallbackService(t, true)
	key, headers := boundKey(t, s, "legacy-rotation")
	bindOneAccount(t, s, key.ID, "missing")
	candidates := []AuthPickCandidate{
		{ID: "low", Provider: apiProvider, Priority: -1},
		{ID: "high", Provider: apiProvider, Priority: 10},
	}
	seen := map[string]int{}
	for range 4 {
		id, handled, err := s.PickAuthForKey(context.Background(), headers, candidates, "model")
		if err != nil || !handled {
			t.Fatalf("legacy pick = (%q, %v, %v)", id, handled, err)
		}
		seen[id]++
	}
	if seen["low"] != 2 || seen["high"] != 2 {
		t.Fatalf("legacy rotation changed: %v", seen)
	}
}

func TestMultiFallbackRotatesCredentialsNotProviders(t *testing.T) {
	s := fallbackService(t, true)
	other := "openai-compatible-other"
	s.cfg.APIFallback.Providers = []string{apiProvider, other}
	key, headers := boundKey(t, s, "credential-rotation")
	bindOneAccount(t, s, key.ID, "missing")
	candidates := []AuthPickCandidate{
		{ID: "a1", Provider: apiProvider, Priority: 2},
		{ID: "a2", Provider: apiProvider, Priority: 2},
		{ID: "a3", Provider: apiProvider, Priority: 2},
		{ID: "b1", Provider: other, Priority: 2},
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
			t.Fatalf("credential rotation changed: %v", seen)
		}
	}
}

func TestFallbackAPIAllowlistAndEqualPriorityRotation(t *testing.T) {
	s := fallbackService(t, true)
	s.cfg.APIFallback.Providers = []string{" OPENAI-COMPATIBLE-AGNES "}
	ctx := context.Background()
	key, headers := boundKey(t, s, "allowed")
	bindOneAccount(t, s, key.ID, "bound")
	candidates := []AuthPickCandidate{
		{ID: "not-allowed", Provider: "openai-compatible-other", Priority: 100},
		{ID: apiAccount1, Provider: apiProvider, Priority: 2},
		{ID: apiAccount2, Provider: apiProvider, Priority: 2},
	}
	seen := map[string]int{}
	for range 4 {
		id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "model")
		if err != nil || !handled {
			t.Fatalf("allowed pick = (%q, %v, %v)", id, handled, err)
		}
		seen[id]++
	}
	if seen[apiAccount1] != 2 || seen[apiAccount2] != 2 || seen["not-allowed"] != 0 {
		t.Fatalf("rotation/allowlist = %v", seen)
	}
	id, handled, err := s.PickAuthForKey(ctx, headers, candidates[:1], "model")
	if id != "" || !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("disallowed-only must fail closed = (%q, %v, %v)", id, handled, err)
	}
	// The allowlist restricts only fallback, not the key's explicit account grant.
	id, handled, err = s.PickAuthForKey(ctx, headers, append(candidates, AuthPickCandidate{ID: "bound", Provider: "codex"}), "model")
	if id != "bound" || !handled || err != nil {
		t.Fatalf("bound grant changed = (%q, %v, %v)", id, handled, err)
	}
}
