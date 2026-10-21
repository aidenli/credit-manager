package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

const magpieProvider = "openai-compatible-magpie"
const deepseekProvider = "openai-compatible-deepseek"

func multiFallbackService(t *testing.T) (*service.Service, string) {
	t.Helper()
	service.Shutdown()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.APIFallback.Providers = []string{deepseekProvider, magpieProvider}
	cfg.APIFallback.Model = "credit-fallback"
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(svc)
	t.Cleanup(service.Shutdown)
	if _, err := svc.UpdateAuthFallbackSettings(context.Background(), store.AuthFallbackSettings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return svc, mintFallbackTestKey(t, context.Background(), svc)
}

func fallbackHostFiles() any {
	return map[string]any{"files": []pluginapi.HostAuthFileEntry{
		{ID: "magpie", Provider: magpieProvider, Priority: 2},
		{ID: "deepseek", Provider: deepseekProvider, Priority: 1},
		{ID: "other-oauth", Provider: "codex", Priority: 100},
		{ID: "other-api", Provider: "openai-compatible-other", Priority: 99},
	}}
}

func TestHostAPIFallbackOrder(t *testing.T) {
	old := HostCall
	t.Cleanup(func() { HostCall = old })
	files := []pluginapi.HostAuthFileEntry{
		{Provider: "codex", Priority: 999},
		{Provider: "openai-compatible-other", Priority: 999},
		{Provider: magpieProvider, Priority: -2},
		{Provider: magpieProvider, Priority: -4},
		{Provider: deepseekProvider, Priority: -3},
		{Provider: deepseekProvider, Priority: 99, Disabled: true},
		{Provider: deepseekProvider, Priority: 100, Status: "disabled"},
	}
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("unexpected method %s", method)
		}
		encoded, err := okEnvelope(map[string]any{"files": files})
		return encoded, 0, err
	}
	allowed := []string{deepseekProvider, magpieProvider}
	order, err := hostAPIFallbackOrder(allowed, "codex")
	if err != nil || !reflect.DeepEqual(order, []string{magpieProvider, deepseekProvider}) {
		t.Fatalf("negative priorities/disabled/allowlist: %v, %v", order, err)
	}
	order, err = hostAPIFallbackOrder(allowed, magpieProvider)
	if err != nil || !reflect.DeepEqual(order, []string{deepseekProvider}) {
		t.Fatalf("original API must not be retried: %v, %v", order, err)
	}
	// Config-defined API providers are absent from host.auth.list: keep config order.
	files = []pluginapi.HostAuthFileEntry{{Provider: "codex", Priority: 2}}
	order, err = hostAPIFallbackOrder(allowed, "codex")
	if err != nil || !reflect.DeepEqual(order, []string{deepseekProvider, magpieProvider}) {
		t.Fatalf("unlisted allowlisted providers: %v, %v", order, err)
	}
	order, _ = hostAPIFallbackOrder(allowed, magpieProvider)
	if !reflect.DeepEqual(order, []string{deepseekProvider}) {
		t.Fatalf("unlisted current provider must not be retried: %v", order)
	}
	files = []pluginapi.HostAuthFileEntry{{Provider: deepseekProvider, Disabled: true}}
	order, _ = hostAPIFallbackOrder(allowed, "codex")
	if !reflect.DeepEqual(order, []string{magpieProvider}) {
		t.Fatalf("listed-disabled provider must stay excluded: %v", order)
	}
	files = []pluginapi.HostAuthFileEntry{
		{Provider: magpieProvider}, {Provider: deepseekProvider},
	}
	first, _ := hostAPIFallbackOrder(allowed, "codex")
	second, _ := hostAPIFallbackOrder(allowed, "codex")
	if len(first) != 2 || len(second) != 2 || first[0] == second[0] {
		t.Fatalf("equal-priority rotation: %v then %v", first, second)
	}
}

func TestMultiFallbackHostListFailureDoesNotUseLegacyProvider(t *testing.T) {
	svc, _ := multiFallbackService(t)
	old := HostCall
	t.Cleanup(func() { HostCall = old })
	for _, malformed := range []bool{false, true} {
		HostCall = func(method string, raw []byte) ([]byte, int, error) {
			if method == pluginabi.MethodHostLog {
				encoded, err := okEnvelope(map[string]any{})
				return encoded, 0, err
			}
			if malformed {
				return []byte(`{"ok":true,"result":"not a file list"}`), 0, nil
			}
			return nil, 1, errors.New("host list failed")
		}
		req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "original", AuthProvider: "codex", AuthID: "bound"}}
		body := []byte(`{"model":"original"}`)
		fb := newAPIFallback(svc, store.PluginKey{})
		for range 2 {
			if fb.take(context.Background(), &req, &body, "upstream status 503") || req.Model != "original" || req.AuthID != "bound" {
				t.Fatalf("list failure changed request: %+v", req)
			}
		}
	}
}

func TestExecuteMultiFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
		attempts []string
	}{
		{"recovers", []int{503, 429, 200}, []string{"codex", magpieProvider, deepseekProvider}},
		{"all exhausted", []int{503, 429, 502}, []string{"codex", magpieProvider, deepseekProvider}},
		{"nonretryable API", []int{503, 401}, []string{"codex", magpieProvider}},
		{"nonretryable OAuth", []int{400}, []string{"codex"}},
		{"uncommon server status", []int{501, 507, 200}, []string{"codex", magpieProvider, deepseekProvider}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, key := multiFallbackService(t)
			old := HostCall
			t.Cleanup(func() { HostCall = old })
			var attempts []string
			lists := 0
			HostCall = func(method string, raw []byte) ([]byte, int, error) {
				var result any = map[string]any{}
				switch method {
				case pluginabi.MethodHostAuthList:
					lists++
					result = fallbackHostFiles()
				case pluginabi.MethodHostModelExecute:
					var req hostModelExecutionRequest
					if err := json.Unmarshal(raw, &req); err != nil {
						return nil, 0, err
					}
					attempts = append(attempts, req.ForcedProvider)
					if len(attempts) > len(tc.statuses) {
						t.Fatalf("unbounded retry: %v", attempts)
					}
					if len(attempts) > 1 && (req.Model != "credit-fallback" || req.AuthID != "" || strings.Contains(string(req.Body), "encrypted_content")) {
						t.Fatalf("unsafe fallback request: %+v", req)
					}
					result = pluginapi.HostModelExecutionResponse{StatusCode: tc.statuses[len(attempts)-1], Body: []byte(`{"choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)}
				case pluginabi.MethodHostLog:
				default:
					return nil, 0, fmt.Errorf("unexpected method %s", method)
				}
				encoded, err := okEnvelope(result)
				return encoded, 0, err
			}
			raw, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "original", SourceFormat: "openai", Format: "openai", AuthProvider: "codex", AuthID: "bound",
				Headers: http.Header{"Authorization": []string{"Bearer " + key}},
				Payload: []byte(`{"model":"original","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`),
			}})
			if _, err := execute(raw); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(attempts, tc.attempts) {
				t.Fatalf("attempts %v, want %v", attempts, tc.attempts)
			}
			if lists > 1 {
				t.Fatalf("priority snapshot read %d times", lists)
			}
			status, err := svc.AuthFallbackStatus(context.Background())
			if err != nil || status.TotalHits != int64(len(attempts)-1) {
				t.Fatalf("fallback audits %+v, %v", status, err)
			}
			if status.LastHit != nil && status.LastHit.Model != "original" {
				t.Fatalf("audit lost original model: %+v", status.LastHit)
			}
		})
	}
}

func TestStreamMultiFallback(t *testing.T) {
	for _, tc := range []struct {
		name, initial string
		partial       bool
		want          []string
	}{
		{"OAuth to API chain", "codex", false, []string{"codex", magpieProvider, deepseekProvider}},
		{"allowed API to API", magpieProvider, false, []string{magpieProvider, deepseekProvider}},
		{"unlisted API stops", "openai-compatible-other", false, []string{"openai-compatible-other"}},
		{"partial output stops", "codex", true, []string{"codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, key := multiFallbackService(t)
			old := HostCall
			t.Cleanup(func() { HostCall = old })
			var attempts []string
			reads := 0
			HostCall = func(method string, raw []byte) ([]byte, int, error) {
				var result any = map[string]any{}
				switch method {
				case pluginabi.MethodHostAuthList:
					result = fallbackHostFiles()
				case pluginabi.MethodHostModelExecuteStream:
					var req hostModelExecutionRequest
					_ = json.Unmarshal(raw, &req)
					attempts = append(attempts, req.ForcedProvider)
					if len(attempts) > 3 {
						t.Fatalf("unbounded retry: %v", attempts)
					}
					reads = 0
					result = pluginapi.HostModelStreamResponse{StreamID: fmt.Sprint(len(attempts)), StatusCode: 200}
				case pluginabi.MethodHostModelStreamRead:
					reads++
					switch {
					case attempts[len(attempts)-1] == deepseekProvider && reads == 1:
						result = pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"type":"response.completed"}`)}
					case attempts[len(attempts)-1] == deepseekProvider:
						result = pluginapi.HostModelStreamReadResponse{Done: true}
					case reads == 1:
						payload := `data: {"type":"response.created"}`
						if tc.partial {
							payload = `data: {"type":"response.output_text.delta","delta":"already sent"}`
						}
						result = pluginapi.HostModelStreamReadResponse{Payload: []byte(payload)}
					default:
						result = pluginapi.HostModelStreamReadResponse{Error: "server_is_overloaded"}
					}
				case pluginabi.MethodHostModelStreamClose, pluginabi.MethodHostStreamEmit, pluginabi.MethodHostLog:
				default:
					return nil, 0, fmt.Errorf("unexpected method %s", method)
				}
				encoded, err := okEnvelope(result)
				return encoded, 0, err
			}
			err := runStream(context.Background(), svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "original", SourceFormat: "openai-response", Format: "openai-response", Stream: true,
				AuthProvider: tc.initial, AuthID: "original-account", Headers: http.Header{"Authorization": []string{"Bearer " + key}},
				Payload: []byte(`{"model":"original","stream":true,"input":"hi","max_output_tokens":16}`),
			}}, "downstream", "")
			if !reflect.DeepEqual(attempts, tc.want) {
				t.Fatalf("attempts %v, want %v; error %v", attempts, tc.want, err)
			}
			if (err != nil) != (tc.partial || strings.Contains(tc.name, "stops")) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
