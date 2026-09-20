package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// affinityService builds a service with two bound accounts for one key, so the
// session-affinity behaviour can be exercised against the real pick path.
func affinityService(t *testing.T, enabled bool) (*Service, store.PluginKey, http.Header) {
	t.Helper()
	s := quotaService(t)
	// Exercise the same path the console uses: persist, then refresh the runtime cache.
	if _, err := s.UpdateAuthSessionAffinitySettings(context.Background(), store.SessionAffinitySettings{
		Enabled: enabled,
		TTL:     time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	key, material, err := s.MintKeyWithPolicy(context.Background(), MintKeyRequest{Label: "affinity"})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+material.Plaintext)
	return s, key, headers
}

func bindTwo(t *testing.T, s *Service, keyID string) []AuthPickCandidate {
	t.Helper()
	if err := s.Store().ReplaceKeyAuthBindings(context.Background(), keyID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
		{Provider: "codex", AuthID: "account-2"},
	}); err != nil {
		t.Fatal(err)
	}
	return []AuthPickCandidate{
		{ID: "account-1", Provider: "codex"},
		{ID: "account-2", Provider: "codex"},
	}
}

func sessionHeaders(key http.Header, session string) http.Header {
	out := http.Header{}
	for k, v := range key {
		out[k] = append([]string(nil), v...)
	}
	if session != "" {
		out.Set("Session-Id", session)
	}
	return out
}

// With affinity off, consecutive picks must keep rotating per request.
func TestSessionAffinityDisabledKeepsRequestRotation(t *testing.T) {
	s, key, headers := affinityService(t, false)
	candidates := bindTwo(t, s, key.ID)
	ctx := context.Background()
	// Same session header must not matter while the feature is off.
	session := sessionHeaders(headers, "session-a")
	var got []string
	for i := 0; i < 4; i++ {
		id, handled, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
		if err != nil || !handled {
			t.Fatalf("pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		got = append(got, id)
	}
	if got[0] != "account-1" || got[1] != "account-2" || got[2] != "account-1" || got[3] != "account-2" {
		t.Fatalf("rotation with affinity disabled = %v", got)
	}
}

// With affinity on, one session must stay on its first account.
func TestSessionAffinityPinsOneSessionToOneAccount(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	ctx := context.Background()
	session := sessionHeaders(headers, "session-a")

	first, handled, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
	if err != nil || !handled {
		t.Fatalf("first pick = (%q, %t, %v)", first, handled, err)
	}
	for i := 0; i < 5; i++ {
		id, handled, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
		if err != nil || !handled {
			t.Fatalf("pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		if id != first {
			t.Fatalf("session drifted: first=%q then=%q", first, id)
		}
	}
}

// Different sessions must still be spread across the bound accounts.
func TestSessionAffinitySpreadsDistinctSessions(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	ctx := context.Background()

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		session := sessionHeaders(headers, "session-"+string(rune('a'+i)))
		id, handled, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
		if err != nil || !handled {
			t.Fatalf("session %d pick = (%q, %t, %v)", i, id, handled, err)
		}
		counts[id]++
	}
	if counts["account-1"] != 2 || counts["account-2"] != 2 {
		t.Fatalf("sessions not spread across accounts: %#v", counts)
	}
}

// Without a session identifier the feature must not engage at all.
func TestSessionAffinityWithoutSessionIDRotates(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	ctx := context.Background()
	// Strip the bearer token's session clues: no Session-Id / X-Client-Request-Id.
	bare := http.Header{}
	bare.Set("Authorization", headers.Get("Authorization"))

	var got []string
	for i := 0; i < 2; i++ {
		id, handled, err := s.PickAuthForKey(ctx, bare, candidates, "gpt-5")
		if err != nil || !handled {
			t.Fatalf("pick %d = (%q, %t, %v)", i, id, handled, err)
		}
		got = append(got, id)
	}
	if got[0] == got[1] {
		t.Fatalf("no session id should still rotate, got %v", got)
	}
}

// A bound account that becomes unusable must not strand the session.
func TestSessionAffinityRebindsWhenBoundAccountUnavailable(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	ctx := context.Background()
	session := sessionHeaders(headers, "session-a")

	first, _, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	// Make the pinned account unusable by capping it at one in-flight request.
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", first, 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-affinity", "gpt-5")
	if err := s.AdmitAuth(ctx, "res-affinity", store.AuthIdentity{AuthID: first, Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	next, handled, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
	if err != nil || !handled {
		t.Fatalf("rebind pick = (%q, %t, %v)", next, handled, err)
	}
	if next == first {
		t.Fatalf("session stayed on the saturated account %q", first)
	}
	// And the session must stick to the new account afterwards.
	again, _, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if again != next {
		t.Fatalf("session did not re-pin: next=%q again=%q", next, again)
	}
}

// An expired binding must be ignored.
func TestSessionAffinityExpiryFallsBackToRotation(t *testing.T) {
	s, key, headers := affinityService(t, true)
	candidates := bindTwo(t, s, key.ID)
	ctx := context.Background()
	session := sessionHeaders(headers, "session-a")

	first, _, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	// Expire every binding in place.
	s.authMu.Lock()
	for k, v := range s.sessionAffinity {
		v.expires = time.Now().Add(-time.Minute)
		s.sessionAffinity[k] = v
	}
	s.authMu.Unlock()

	next, _, err := s.PickAuthForKey(ctx, session, candidates, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if next == first {
		t.Fatalf("expired binding still honored: %q", next)
	}
}

// Session identity must follow the host's header precedence.
func TestSessionAffinityIDPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"claude", map[string]string{"X-Claude-Code-Session-Id": "c1"}, "claude:c1"},
		{"codex-session-id", map[string]string{"Session-Id": "s1"}, "codex:s1"},
		{"codex-session_id", map[string]string{"Session_id": "s2"}, "codex:s2"},
		{"generic", map[string]string{"X-Session-ID": "s3"}, "header:s3"},
		{"affinity", map[string]string{"X-Session-Affinity": "s4"}, "affinity:s4"},
		{"client-request", map[string]string{"X-Client-Request-Id": "s5"}, "clientreq:s5"},
		{"none", map[string]string{}, ""},
		{
			"claude wins over codex",
			map[string]string{"X-Claude-Code-Session-Id": "c9", "Session-Id": "s9"},
			"claude:c9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			for k, v := range tc.headers {
				headers.Set(k, v)
			}
			if got := sessionAffinityID(headers); got != tc.want {
				t.Fatalf("sessionAffinityID = %q, want %q", got, tc.want)
			}
		})
	}
}

// The binding key must include model and provider, mirroring the host.
func TestSessionAffinityKeyIncludesProviderAndModel(t *testing.T) {
	if got, want := sessionAffinityKey("codex", "codex:s1", "gpt-5"), "codex::codex:s1::gpt-5"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
}

// Unbound keys must be untouched by the feature.
func TestSessionAffinityDoesNotAffectUnboundKeys(t *testing.T) {
	s := quotaService(t)
	s.setSessionAffinityRuntime(true, time.Hour)
	_, material, err := s.MintKeyWithPolicy(context.Background(), MintKeyRequest{Label: "unbound"})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+material.Plaintext)
	candidates := []AuthPickCandidate{{ID: "account-1", Provider: "codex"}, {ID: "account-2", Provider: "codex"}}
	id, handled, err := s.PickAuthForKey(context.Background(), sessionHeaders(headers, "session-a"), candidates, "gpt-5")
	if err != nil || handled || id != "" {
		t.Fatalf("unbound key must defer to the host scheduler, got (%q, %t, %v)", id, handled, err)
	}
}
