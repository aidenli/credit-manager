package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

func bindingsTestService(t *testing.T) *service.Service {
	t.Helper()
	t.Setenv("CREDIT_MANAGER_TEST_PEPPERS", "active:0123456789abcdef0123456789abcdef")
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Keys.PepperEnv = "CREDIT_MANAGER_TEST_PEPPERS"
	cfg.Keys.ActivePepperID = "active"
	svc, err := service.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func updateKeyBody(t *testing.T, svc *service.Service, keyID, body string) map[string]any {
	t.Helper()
	resp, err := updateKey(context.Background(), svc, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func storedBindings(t *testing.T, svc *service.Service, keyID string) []store.KeyAuthBinding {
	t.Helper()
	bindings, err := svc.Store().ListKeyAuthBindings(context.Background(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	return bindings
}

func TestKeyAuthBindingsRequireProviderAndAuthID(t *testing.T) {
	if _, err := keyAuthBindings([]bindingInput{{Provider: "codex"}}); err == nil {
		t.Fatal("missing auth_id must be rejected")
	}
	if _, err := keyAuthBindings([]bindingInput{{AuthID: "account-a"}}); err == nil {
		t.Fatal("missing provider must be rejected")
	}
	got, err := keyAuthBindings([]bindingInput{{Provider: " codex ", AuthID: " account-a "}})
	if err != nil || len(got) != 1 || got[0].Provider != "codex" || got[0].AuthID != "account-a" {
		t.Fatalf("bindings = %#v err=%v", got, err)
	}
}

func TestKeyViewCarriesAuthBindings(t *testing.T) {
	view := keyViewWithBindings(store.PluginKey{ID: "key-1"}, []store.KeyAuthBinding{{Provider: "codex", AuthID: "account-a"}})
	bindings, ok := view["auth_bindings"].([]map[string]any)
	if !ok || len(bindings) != 1 || bindings[0]["auth_id"] != "account-a" || bindings[0]["provider"] != "codex" {
		t.Fatalf("auth_bindings = %#v", view["auth_bindings"])
	}
	if empty := keyViewWithBindings(store.PluginKey{ID: "key-2"}, nil); len(empty["auth_bindings"].([]map[string]any)) != 0 {
		t.Fatalf("empty auth_bindings = %#v", empty["auth_bindings"])
	}
}

func TestCreateKeyAppliesAuthBindings(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	resp, err := createKey(ctx, svc, []byte(`{
		"label":"bound",
		"auth_bindings":[{"provider":" Codex ","auth_id":"account-a"},{"provider":"codex","auth_id":"account-a"},{"provider":"codex","auth_id":"account-b"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	bindings, ok := view["auth_bindings"].([]any)
	if !ok || len(bindings) != 2 {
		t.Fatalf("created auth_bindings = %#v", view["auth_bindings"])
	}
	if got := storedBindings(t, svc, fmt.Sprint(view["id"])); len(got) != 2 {
		t.Fatalf("stored bindings = %#v", got)
	}

	resp, err = createKey(ctx, svc, []byte(`{"label":"broken","auth_bindings":[{"provider":"codex"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("incomplete binding status = %d", resp.StatusCode)
	}
}

func TestUpdateKeyAuthBindingFieldSemantics(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "bound", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	view := updateKeyBody(t, svc, key.ID, fmt.Sprintf(
		`{"id":%q,"auth_bindings":[{"provider":"codex","auth_id":"account-a"},{"provider":"codex","auth_id":"account-b"}]}`, key.ID))
	if len(view["auth_bindings"].([]any)) != 2 {
		t.Fatalf("after set = %#v", view["auth_bindings"])
	}

	// Omitted field keeps the current bindings.
	view = updateKeyBody(t, svc, key.ID, fmt.Sprintf(`{"id":%q,"label":"renamed"}`, key.ID))
	if len(view["auth_bindings"].([]any)) != 2 || len(storedBindings(t, svc, key.ID)) != 2 {
		t.Fatalf("omitted field changed bindings: %#v", view["auth_bindings"])
	}

	// Explicit empty array clears them and restores default scheduling.
	view = updateKeyBody(t, svc, key.ID, fmt.Sprintf(`{"id":%q,"auth_bindings":[]}`, key.ID))
	if len(view["auth_bindings"].([]any)) != 0 || len(storedBindings(t, svc, key.ID)) != 0 {
		t.Fatalf("empty array did not clear bindings: %#v", view["auth_bindings"])
	}

	// A rejected payload must not silently drop what is already stored.
	updateKeyBody(t, svc, key.ID, fmt.Sprintf(
		`{"id":%q,"auth_bindings":[{"provider":"codex","auth_id":"account-c"}]}`, key.ID))
	resp, err := updateKey(ctx, svc, []byte(fmt.Sprintf(`{"id":%q,"auth_bindings":[{"auth_id":"account-d"}]}`, key.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid binding status = %d", resp.StatusCode)
	}
	if got := storedBindings(t, svc, key.ID); len(got) != 1 || got[0].AuthID != "account-c" {
		t.Fatalf("rejected update changed bindings = %#v", got)
	}
}

func TestRotateKeyKeepsAuthBindings(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "bound", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().ReplaceKeyAuthBindings(ctx, key.ID, []store.KeyAuthBinding{{Provider: "codex", AuthID: "account-a"}}); err != nil {
		t.Fatal(err)
	}
	rotated, _, err := svc.RotateKey(ctx, key.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	got := storedBindings(t, svc, rotated.ID)
	if len(got) != 1 || got[0].AuthID != "account-a" {
		t.Fatalf("rotated key bindings = %#v", got)
	}
}

func TestConsoleKeyAuthBindingControls(t *testing.T) {
	page := strings.ReplaceAll(string(consolePage().Body), "\r\n", "\n")
	for _, text := range []string{
		`id="keyModalAuthBindings" multiple`,
		`id="keyModalAuthBindingFilter"`,
		`id="btnKeyModalClearBindings"`,
		"function renderKeyAuthBindings",
		"function selectedKeyAuthBindings",
		"function keyAuthBindingsPayload",
		"function clearKeyAuthBindings",
		"auth_bindings",
		"未选择表示不限制；选择后若全部不可用，请求将失败，不会使用其他账户。",
		"loadKeyAuthBindings",
	} {
		if !strings.Contains(page, text) {
			t.Fatalf("console key modal is missing auth bindings: %q", text)
		}
	}
	// Without a loaded account list the modal must not send an empty binding set.
	if !strings.Contains(page, "if (!state.keyAuthAccountsLoaded) return {};") {
		t.Fatal("auth binding payload does not stay inert when accounts failed to load")
	}
}

// A key may be bound to an API-key provider, not only an OAuth account, and the
// picker must say which is which. The list therefore comes from the host's full
// credential set (auth-accounts), not from auth-quotas, which omits API providers.
func TestConsoleBindingPickerOffersBothAccountKinds(t *testing.T) {
	page := strings.ReplaceAll(string(consolePage().Body), "\r\n", "\n")
	for _, text := range []string{
		"credit-manager/auth-accounts",
		"async function loadKeyAuthAccounts",
		"function bindingTypeMark",
		"account.oauth === true",
		// The visible type prefix.
		"'[' + bindingTypeMark(account) + '] '",
		"oauth = OAuth 账号，api = API 提供商",
	} {
		if !strings.Contains(page, text) {
			t.Fatalf("binding picker is missing account-type support: %q", text)
		}
	}
	// It must no longer source the key modal from the quota-derived list.
	if strings.Contains(page, "state.keyAuthAccounts = (await loadAuthWarmupAuths())") {
		t.Fatal("binding picker still sources accounts from the auth-quotas list")
	}
}
