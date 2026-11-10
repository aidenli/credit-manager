package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestPickAuthSessionRoutingLogIsOptionalAndBestEffort(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			service.Shutdown()
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			cfg.SessionAffinity.LogDecisions = enabled
			svc, err := service.Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			service.Replace(svc)
			t.Cleanup(service.Shutdown)
			_, material, err := svc.MintKeyWithPolicy(context.Background(), service.MintKeyRequest{Label: "monitor"})
			if err != nil {
				t.Fatal(err)
			}
			oldHost := HostCall
			t.Cleanup(func() { HostCall = oldHost })
			var messages []string
			HostCall = func(method string, raw []byte) ([]byte, int, error) {
				if method != pluginabi.MethodHostLog {
					t.Fatalf("unexpected callback %s", method)
				}
				var req struct{ Message string }
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Fatal(err)
				}
				messages = append(messages, req.Message)
				return nil, 1, errors.New("logger unavailable")
			}
			raw, _ := json.Marshal(map[string]any{
				"Model": "gpt-5", "Options": map[string]any{"Headers": map[string][]string{"Authorization": {"Bearer " + material.Plaintext}, "Session-Id": {"private-chat"}}},
				"Candidates": []map[string]any{{"ID": "private-account", "Provider": "codex"}},
			})
			out, err := pickAuthWithLifecycleTest(t, raw)
			if err != nil || !strings.Contains(string(out), `"AuthID":"private-account"`) {
				t.Fatalf("logging changed routing result: %s %v", out, err)
			}
			if !enabled && len(messages) != 0 {
				t.Fatal("disabled monitor emitted a log")
			}
			if enabled {
				if len(messages) != 1 || !strings.HasPrefix(messages[0], "credit-manager session-routing ") {
					t.Fatalf("unexpected log: %v", messages)
				}
				var d service.SessionRoutingDecision
				if err := json.Unmarshal([]byte(strings.TrimPrefix(messages[0], "credit-manager session-routing ")), &d); err != nil || d.Reason != "affinity_disabled" {
					t.Fatalf("unparseable decision: %v %v", messages, err)
				}
				if strings.Contains(messages[0], "private-chat") || strings.Contains(messages[0], "private-account") || strings.Contains(messages[0], material.Plaintext) {
					t.Fatal("raw identity leaked into log")
				}
			}
		})
	}
}

func TestAuthPickErrorCodeSeparatesBoundFailure(t *testing.T) {
	if got := authPickErrorCode(service.ErrNoBoundAuthAvailable); got != "bound_auth_unavailable" {
		t.Fatalf("bound error code = %q", got)
	}
	if got := authPickErrorCode(store.ErrConcurrentLimit); got != "limit_rejected" {
		t.Fatalf("limit error code = %q", got)
	}
}

// The host's per-candidate status must survive the conversion into the plugin's
// candidate type, because an explicitly disabled account must still be refused.
//
// account-1 is bound and disabled, account-2 is healthy but NOT bound: the pick
// must fail closed rather than fall back to an account the key was not granted. An
// "error" flag is advisory and no longer refuses a bound account (it would strand
// the account permanently), so this uses "disabled".
func TestPickAuthForwardsCandidateStatus(t *testing.T) {
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(func() { service.Shutdown() })

	key, material, err := svc.MintKeyWithPolicy(context.Background(), service.MintKeyRequest{Label: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().ReplaceKeyAuthBindings(context.Background(), key.ID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		"model": "gpt-5",
		"options": map[string]any{
			"headers": map[string][]string{"Authorization": {"Bearer " + material.Plaintext}},
		},
		"candidates": []map[string]any{
			{"ID": "account-1", "Provider": "codex", "Status": "disabled"},
			{"ID": "account-2", "Provider": "codex", "Status": "active"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := pickAuthWithLifecycleTest(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "bound_auth_unavailable") {
		t.Fatalf("pickAuth should fail closed on a disabled bound account, got %s", out)
	}
}

// An errored bound account is still selected over an unbound healthy one: the
// binding is the access boundary, and leaving the account unused would keep the
// host's error flag set forever.
func TestPickAuthUsesErroredBoundAccountNotUnboundSibling(t *testing.T) {
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(func() { service.Shutdown() })

	key, material, err := svc.MintKeyWithPolicy(context.Background(), service.MintKeyRequest{Label: "errored"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().ReplaceKeyAuthBindings(context.Background(), key.ID, []store.KeyAuthBinding{
		{Provider: "codex", AuthID: "account-1"},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		"model": "gpt-5",
		"options": map[string]any{
			"headers": map[string][]string{"Authorization": {"Bearer " + material.Plaintext}},
		},
		"candidates": []map[string]any{
			{"ID": "account-1", "Provider": "codex", "Status": "error"},
			{"ID": "account-2", "Provider": "codex", "Status": "active"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := pickAuthWithLifecycleTest(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"AuthID":"account-1"`) {
		t.Fatalf("pickAuth must stay inside the binding, got %s", out)
	}
}
