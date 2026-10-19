package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
