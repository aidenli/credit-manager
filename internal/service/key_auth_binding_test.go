package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

// boundKey mints a real plugin key so its plaintext can travel in the same
// Authorization header the host forwards to the scheduler.
func boundKey(t *testing.T, s *Service, label string) (store.PluginKey, http.Header) {
	t.Helper()
	key, material, err := s.MintKeyWithPolicy(context.Background(), MintKeyRequest{Label: label})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+material.Plaintext)
	return key, headers
}

func TestPickAuthForKeyRestrictsToBoundAccounts(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "bound")
	// "openai" must normalise onto the host's "codex" provider.
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{{Provider: "openai", AuthID: "account-2"}}); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: "account-2", Provider: "codex"},
		{ID: "account-3", Provider: "codex"},
	}
	for i := 0; i < 3; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5")
		if err != nil || !handled || id != "account-2" {
			t.Fatalf("bound pick %d = (%q, %t, %v)", i, id, handled, err)
		}
	}
}

func TestPickAuthForKeyFailsClosedWhenNoBoundAccountIsUsable(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()

	missing, missingHeaders := boundKey(t, s, "missing")
	if err := s.Store().ReplaceKeyAuthBindings(ctx, missing.ID, []store.KeyAuthBinding{{Provider: "codex", AuthID: "account-9"}}); err != nil {
		t.Fatal(err)
	}
	id, handled, err := s.PickAuthForKey(ctx, missingHeaders, []AuthPickCandidate{{ID: "account-1", Provider: "codex"}}, "gpt-5")
	if !handled || id != "" || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("unlisted bound pick = (%q, %t, %v)", id, handled, err)
	}

	busy, busyHeaders := boundKey(t, s, "busy")
	if err := s.Store().ReplaceKeyAuthBindings(ctx, busy.ID, []store.KeyAuthBinding{{Provider: "codex", AuthID: "account-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "account-1", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-busy", "gpt")
	if err := s.AdmitAuth(ctx, "res-busy", store.AuthIdentity{AuthID: "account-1", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	// account-2 is free but not bound, so it must not be used as a fallback.
	_, handled, err = s.PickAuthForKey(ctx, busyHeaders, []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: "account-2", Provider: "codex"},
	}, "gpt-5")
	if !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
		t.Fatalf("busy bound pick = handled:%t err:%v", handled, err)
	}
}

func TestPickAuthForKeyKeepsUnboundBehaviour(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	_, headers := boundKey(t, s, "unbound")
	candidates := []AuthPickCandidate{{ID: "account-1", Provider: "codex"}, {ID: "account-2", Provider: "codex"}}
	if id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5"); err != nil || handled || id != "" {
		t.Fatalf("unlimited unbound pick = (%q, %t, %v)", id, handled, err)
	}
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "account-1", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-unbound", "gpt")
	if err := s.AdmitAuth(ctx, "res-unbound", store.AuthIdentity{AuthID: "account-1", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5")
	if err != nil || !handled || id != "account-2" {
		t.Fatalf("limited unbound pick = (%q, %t, %v)", id, handled, err)
	}
}

func TestPickAuthForKeyConcurrentRotationIsSafe(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "concurrent")
	bindings := []store.KeyAuthBinding{{Provider: "codex", AuthID: "account-1"}, {Provider: "codex", AuthID: "account-2"}}
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, bindings); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{{ID: "account-1", Provider: "codex"}, {ID: "account-2", Provider: "codex"}}
	const calls = 40
	results := make(chan string, calls)
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5")
			if err != nil || !handled {
				errs <- err
				return
			}
			results <- id
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	if len(errs) != 0 {
		t.Fatalf("concurrent picks failed: %v", <-errs)
	}
	counts := map[string]int{}
	for id := range results {
		counts[id]++
	}
	if counts["account-1"] != calls/2 || counts["account-2"] != calls/2 {
		t.Fatalf("concurrent rotation counts = %#v", counts)
	}
}

func TestPickAuthForKeyRotatesEachKeyIndependently(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	first, firstHeaders := boundKey(t, s, "first")
	second, secondHeaders := boundKey(t, s, "second")
	bindings := []store.KeyAuthBinding{{Provider: "codex", AuthID: "account-1"}, {Provider: "codex", AuthID: "account-2"}}
	if err := s.Store().ReplaceKeyAuthBindings(ctx, first.ID, bindings); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().ReplaceKeyAuthBindings(ctx, second.ID, bindings); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{{ID: "account-1", Provider: "codex"}, {ID: "account-2", Provider: "codex"}}
	for _, tc := range []struct {
		name    string
		headers http.Header
	}{{"first", firstHeaders}, {"second", secondHeaders}} {
		var picked []string
		for i := 0; i < 2; i++ {
			id, handled, err := s.PickAuthForKey(ctx, tc.headers, candidates, "gpt-5")
			if err != nil || !handled {
				t.Fatalf("%s pick %d = (%q, %t, %v)", tc.name, i, id, handled, err)
			}
			picked = append(picked, id)
		}
		// A shared cursor would make the second key start at account-2.
		if picked[0] != "account-1" || picked[1] != "account-2" {
			t.Fatalf("%s rotation = %v", tc.name, picked)
		}
	}
}

// An account the host has marked error/disabled must never be selected, even
// though it is still in the candidate list: the status is a snapshot and the
// account can go bad between list construction and the pick.
// Only an explicit disable fails closed. An advisory "error" flag must NOT black
// out a key's own account: an account can only clear that flag by serving a
// request, so honoring it here strands the key on the fallback forever (and, once
// every account carries the flag, takes the whole provider out of service).
func TestPickAuthForKeyFailsClosedOnlyForDisabledAccounts(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"disabled", "disabled"},
		{"disabled with odd casing", "  DISABLED "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := quotaService(t)
			key, headers := boundKey(t, s, "disabled-status")
			if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
				{Provider: "codex", AuthID: "account-1"},
			}); err != nil {
				t.Fatal(err)
			}
			_, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
				{ID: "account-1", Provider: "codex", Status: tc.status},
			}, "gpt-5")
			if !handled || !errors.Is(err, ErrNoBoundAuthAvailable) {
				t.Fatalf("status %q must fail closed, got handled=%t err=%v", tc.status, handled, err)
			}
		})
	}
}

// An errored account is still used when it is all the key has, which is what lets
// the host's error flag clear again.
func TestPickAuthForKeyUsesErroredAccountWhenItIsTheOnlyOption(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "errored-only")
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
	}); err != nil {
		t.Fatal(err)
	}
	id, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
		{ID: "account-1", Provider: "codex", Status: "error"},
	}, "gpt-5")
	if err != nil || !handled || id != "account-1" {
		t.Fatalf("pick = (%q, %t, %v), want account-1", id, handled, err)
	}
}

// A bad account must be passed over for a healthy sibling in the same binding.
func TestPickAuthForKeyPrefersHealthyBoundAccount(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "mixed-status")
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
		{Provider: "codex", AuthID: "account-2"},
	}); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{
		{ID: "account-1", Provider: "codex", Status: "error"},
		{ID: "account-2", Provider: "codex", Status: "active"},
	}
	for i := 0; i < 3; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5")
		if err != nil || !handled || id != "account-2" {
			t.Fatalf("pick %d = (%q, %t, %v), want account-2", i, id, handled, err)
		}
	}
}

// Only the terminal bad states are rejected: transient and unknown statuses must
// stay usable, otherwise a host that adds a new status value would break routing.
func TestPickAuthForKeyAcceptsNonTerminalStatuses(t *testing.T) {
	for _, status := range []string{"", "active", "pending", "refreshing", "unknown", "some-future-state"} {
		t.Run("status="+status, func(t *testing.T) {
			s := quotaService(t)
			ctx := context.Background()
			key, headers := boundKey(t, s, "ok-status")
			if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
				{Provider: "codex", AuthID: "account-1"},
			}); err != nil {
				t.Fatal(err)
			}
			id, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
				{ID: "account-1", Provider: "codex", Status: status},
			}, "gpt-5")
			if err != nil || !handled || id != "account-1" {
				t.Fatalf("status %q must stay usable, got (%q, %t, %v)", status, id, handled, err)
			}
		})
	}
}

func TestAuthStatusClassification(t *testing.T) {
	for _, status := range []string{"disabled", " DISABLED "} {
		if !authStatusDisabled(status) {
			t.Fatalf("status %q should be a disable", status)
		}
	}
	for _, status := range []string{"", "active", "error", "pending", "refreshing", "unknown", "mystery"} {
		if authStatusDisabled(status) {
			t.Fatalf("status %q must not be treated as a disable", status)
		}
	}
	for _, status := range []string{"error", " ERROR "} {
		if !authStatusErrored(status) {
			t.Fatalf("status %q should be an advisory error", status)
		}
	}
	for _, status := range []string{"", "active", "disabled", "pending", "mystery"} {
		if authStatusErrored(status) {
			t.Fatalf("status %q must not be treated as an advisory error", status)
		}
	}
}
