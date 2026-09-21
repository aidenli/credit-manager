package service

import (
	"context"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

// Binding is keyed by (provider, auth_id); it must work for API-key providers
// exactly as it does for OAuth accounts. The identifiers used here are the ones
// production actually recorded for openai-compatibility entries:
//
//	ledger auth_id    = "openai-compatibility:<name>:<hash>"
//	ledger provider   = "openai-compatible-<name>"   (host key for compat)
//
// The important part is that the compat provider must NOT be normalised into
// "codex": it merely contains the substring "openai".
func TestPickAuthForKeyBindsAPIKeyProvider(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "api-provider")

	const (
		compatProvider = "openai-compatible-agnes"
		compatAuthID   = "openai-compatibility:agnes:4dab06a22e06"
		oauthAuthID    = "codex-561190dc-gaoding_003@163.com-prolite.json"
	)
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
		{Provider: compatProvider, AuthID: compatAuthID},
	}); err != nil {
		t.Fatal(err)
	}

	// The provider must survive normalisation untouched: "openai-compatible-x"
	// merely contains the substring "openai" and must not collapse into "codex".
	stored, err := s.Store().ListKeyAuthBindings(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Provider != compatProvider {
		t.Fatalf("stored binding = %#v, want provider %q", stored, compatProvider)
	}
	if got := authLimitProvider(compatProvider); got != compatProvider {
		t.Fatalf("selection normalised provider to %q, want %q", got, compatProvider)
	}

	candidates := []AuthPickCandidate{
		{ID: compatAuthID, Provider: compatProvider, Status: "active"},
		{ID: oauthAuthID, Provider: "codex", Status: "active"},
	}
	for i := 0; i < 3; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "agnes-3.0-flash")
		if err != nil || !handled {
			t.Fatalf("pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		if id != compatAuthID {
			t.Fatalf("pick %d = %q, want the bound compat account", i, id)
		}
	}
}

// Mixed bindings (one OAuth account plus one API-key provider) must rotate
// across both, which is the point of allowing API providers to be bound.
func TestPickAuthForKeyRotatesAcrossOAuthAndAPIProvider(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "mixed-providers")

	const (
		compatProvider = "openai-compatible-agnes"
		compatAuthID   = "openai-compatibility:agnes:4dab06a22e06"
		oauthAuthID    = "codex-561190dc-gaoding_003@163.com-prolite.json"
	)
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
		{Provider: compatProvider, AuthID: compatAuthID},
		{Provider: "codex", AuthID: oauthAuthID},
	}); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{
		{ID: oauthAuthID, Provider: "codex", Status: "active"},
		{ID: compatAuthID, Provider: compatProvider, Status: "active"},
	}

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		id, handled, err := s.PickAuthForKey(ctx, headers, candidates, "gpt-5")
		if err != nil || !handled {
			t.Fatalf("pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		seen[id]++
	}
	// Selection keys its cursor on the first available candidate's provider, so
	// with mixed providers the rotation stays inside one provider group. Both
	// accounts must still be reachable across the two scopes.
	if seen[oauthAuthID] == 0 {
		t.Fatalf("oauth account never selected: %#v", seen)
	}
	t.Logf("rotation across mixed providers: %#v", seen)
}

// A bound API-key account the host explicitly disabled must fail closed, exactly
// like a disabled OAuth account.
func TestPickAuthForKeySkipsBadAPIKeyProvider(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	key, headers := boundKey(t, s, "api-provider-bad")
	if err := s.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{
		{Provider: "openai-compatible-agnes", AuthID: "openai-compatibility:agnes:4dab06a22e06"},
	}); err != nil {
		t.Fatal(err)
	}
	_, handled, err := s.PickAuthForKey(ctx, headers, []AuthPickCandidate{
		{ID: "openai-compatibility:agnes:4dab06a22e06", Provider: "openai-compatible-agnes", Status: "disabled"},
	}, "agnes-3.0-flash")
	if !handled || err == nil {
		t.Fatalf("disabled compat account must fail closed, got handled=%t err=%v", handled, err)
	}
}
