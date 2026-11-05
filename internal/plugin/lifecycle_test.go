package plugin

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestDecodeLifecycleAcceptsConfigYAMLRepresentations(t *testing.T) {
	yamlConfig := []byte("data_dir: ./data/credit-manager\nbusy_timeout: 5s\n")
	standard, err := json.Marshal(lifecycleRequest{ConfigYAML: yamlConfig, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(map[string]any{
		"config_yaml":    string(yamlConfig),
		"schema_version": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	byteValues := make([]int, len(yamlConfig))
	for i, value := range yamlConfig {
		byteValues[i] = int(value)
	}
	array, err := json.Marshal(map[string]any{
		"config_yaml":    byteValues,
		"schema_version": 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, raw := range map[string][]byte{
		"base64": standard,
		"plain":  plain,
		"array":  array,
	} {
		t.Run(name, func(t *testing.T) {
			request, decodeErr := decodeLifecycle(raw)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if string(request.ConfigYAML) != string(yamlConfig) {
				t.Fatalf("config yaml = %q", request.ConfigYAML)
			}
			if request.SchemaVersion != 1 {
				t.Fatalf("schema = %d", request.SchemaVersion)
			}
		})
	}
}

func TestNegotiateRPCSchemaCapsAtSupportedVersion(t *testing.T) {
	for _, tc := range []struct {
		host, want uint32
	}{
		{0, 1}, {1, 1}, {2, 2}, {6, 6}, {7, 7}, {10, 7},
	} {
		t.Run(fmt.Sprint(tc.host), func(t *testing.T) {
			if got := negotiateRPCSchema(tc.host); got != tc.want {
				t.Fatalf("negotiated schema = %d, want %d", got, tc.want)
			}
			raw, err := okEnvelope(pluginRegistration(negotiateRPCSchema(tc.host)))
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				OK     bool `json:"ok"`
				Result struct {
					SchemaVersion uint32                     `json:"schema_version"`
					Capabilities  map[string]json.RawMessage `json:"capabilities"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if !wire.OK || wire.Result.SchemaVersion != tc.want {
				t.Fatalf("registration = %s", raw)
			}
			caps := wire.Result.Capabilities
			wantFlag := ""
			if tc.want >= 7 {
				wantFlag = "true"
			}
			if got := string(caps["frontend_auth_provider_omit_body"]); got != wantFlag {
				t.Fatalf("omit body flag = %q, want %q", got, wantFlag)
			}
			if got := string(caps["request_interceptor_omit_body_after"]); got != wantFlag {
				t.Fatalf("interceptor after omit body flag = %q, want %q", got, wantFlag)
			}
			for _, name := range []string{"frontend_auth_provider", "frontend_auth_provider_exclusive", "request_interceptor", "request_lifecycle_plugin"} {
				if string(caps[name]) != "true" {
					t.Fatalf("capability %s = %s, want true", name, caps[name])
				}
			}
			for _, name := range []string{"request_interceptor_omit_body_before"} {
				if _, exists := caps[name]; exists {
					t.Fatalf("unexpected interceptor body opt-out: %s", name)
				}
			}
		})
	}
}
