package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
)

func TestSchedulerAcrossPrioritiesSurvivesAuthOnly(t *testing.T) {
	for _, authOnly := range []bool{false, true} {
		name := "executor"
		if authOnly {
			name = "auth_only"
		}
		t.Run(name, func(t *testing.T) {
			service.Shutdown()
			cfg := config.Default()
			cfg.DataDir, cfg.AuthOnly = t.TempDir(), authOnly
			svc, err := service.Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			service.Replace(svc)
			t.Cleanup(service.Shutdown)
			raw, err := json.Marshal(pluginRegistration(supportedRPCSchemaVersion))
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			for _, flag := range []string{"scheduler", "scheduler_across_priorities"} {
				if string(wire.Capabilities[flag]) != "true" {
					t.Fatalf("%s missing from wire registration: %s", flag, raw)
				}
			}
			reg := pluginRegistration(supportedRPCSchemaVersion)
			if reg.Capabilities.Executor == authOnly || reg.Capabilities.ModelRouter == authOnly {
				t.Fatalf("auth_only=%t executor/router registration = %+v", authOnly, reg.Capabilities)
			}
		})
	}
}
