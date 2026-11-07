package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestPickAuthForwardsFallbackPriority(t *testing.T) {
	svc, material := multiFallbackService(t)
	key, _, err := svc.ResolveIdentity(context.Background(), map[string][]string{"Authorization": {"Bearer " + material}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().ReplaceKeyAuthBindings(context.Background(), key.ID, []store.KeyAuthBinding{{Provider: "codex", AuthID: "missing"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"model":   "original",
		"options": map[string]any{"headers": map[string][]string{"Authorization": {"Bearer " + material}}},
		"candidates": []map[string]any{
			{"ID": "lower", "Provider": deepseekProvider, "Priority": -2},
			{"ID": "higher", "Provider": magpieProvider, "Priority": -1},
		},
	})
	out, err := pickAuth(raw)
	if err != nil || !strings.Contains(string(out), `"AuthID":"higher"`) {
		t.Fatalf("scheduler dropped priority: %s, %v", out, err)
	}
}

// The host wire format (no json tags: Options.Headers / Options.Metadata) must
// deliver the host-resolved session so one conversation stays on one account.
func TestPickAuthUsesHostSessionMetadata(t *testing.T) {
	svc, material := multiFallbackService(t)
	key, _, err := svc.ResolveIdentity(context.Background(), map[string][]string{"Authorization": {"Bearer " + material}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().ReplaceKeyAuthBindings(context.Background(), key.ID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
		{Provider: "codex", AuthID: "account-2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateAuthSessionAffinitySettings(context.Background(), store.SessionAffinitySettings{Enabled: true, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	pick := func(session string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"Model": "gpt-5",
			"Options": map[string]any{
				"Headers":  map[string][]string{"Authorization": {"Bearer " + material}},
				"Metadata": map[string]any{"canonical_session_id": session},
			},
			"Candidates": []map[string]any{
				{"ID": "account-1", "Provider": "codex", "Priority": 2},
				{"ID": "account-2", "Provider": "codex", "Priority": 2},
			},
		})
		out, err := pickAuth(raw)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			OK     bool `json:"ok"`
			Result struct{ AuthID string }
		}
		_ = json.Unmarshal(out, &env)
		if env.Result.AuthID == "" {
			t.Fatalf("no auth picked: %s", out)
		}
		return env.Result.AuthID
	}
	first := pick("pck:conv-1")
	for range 4 {
		if got := pick("pck:conv-1"); got != first {
			t.Fatalf("same host session drifted: %q then %q", first, got)
		}
	}
	if other := pick("pck:conv-2"); other == first {
		t.Fatalf("new session should rotate to the sibling account, got %q", other)
	}
}

// The host forwards the credential weight in candidate Attributes; new sessions
// must be spread by it (3:1 here) and weight "0" must exclude the account.
func TestPickAuthUsesHostCredentialWeight(t *testing.T) {
	svc, material := multiFallbackService(t)
	key, _, err := svc.ResolveIdentity(context.Background(), map[string][]string{"Authorization": {"Bearer " + material}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().ReplaceKeyAuthBindings(context.Background(), key.ID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
		{Provider: "codex", AuthID: "account-2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateAuthSessionAffinitySettings(context.Background(), store.SessionAffinitySettings{Enabled: true, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	pick := func(session, weight1, weight2 string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"Model": "gpt-5",
			"Options": map[string]any{
				"Headers":  map[string][]string{"Authorization": {"Bearer " + material}},
				"Metadata": map[string]any{"canonical_session_id": session},
			},
			"Candidates": []map[string]any{
				{"ID": "account-1", "Provider": "codex", "Priority": 2, "Attributes": map[string]string{"weight": weight1}},
				{"ID": "account-2", "Provider": "codex", "Priority": 2, "Attributes": map[string]string{"weight": weight2}},
			},
		})
		out, err := pickAuth(raw)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Result struct{ AuthID string }
		}
		_ = json.Unmarshal(out, &env)
		if env.Result.AuthID == "" {
			t.Fatalf("no auth picked: %s", out)
		}
		return env.Result.AuthID
	}
	counts := map[string]int{}
	for i := range 40 {
		counts[pick(fmt.Sprintf("pck:conv-%d", i), "3", "1")]++
	}
	if counts["account-1"] != 30 || counts["account-2"] != 10 {
		t.Fatalf("weighted sessions = %v, want 30:10", counts)
	}
	for i := range 5 {
		if got := pick(fmt.Sprintf("pck:excluded-%d", i), "0", "1"); got != "account-2" {
			t.Fatalf("weight 0 account selected: %q", got)
		}
	}
}
