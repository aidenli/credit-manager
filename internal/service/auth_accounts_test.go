package service

import (
	"context"
	"testing"
)

// The account picker must offer API-key providers as well as OAuth accounts, and
// say which is which. The type comes from reading the credential: an entry
// without an OAuth access token is API-key backed.
func TestAuthAccountsDistinguishesOAuthFromAPIKey(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files: []AuthQuotaFile{
			{ID: "codex-aaa-x@example.com-pro.json", AuthIndex: "idx-oauth", Provider: "codex", Type: "codex", Email: "x@example.com"},
			{ID: "openai-compatibility:agnes:4dab06a22e06", AuthIndex: "idx-api", Provider: "openai-compatible-agnes", Type: "openai-compatibility", Label: "agnes"},
			{ID: "codex-off@example.com-pro.json", AuthIndex: "idx-off", Provider: "codex", Type: "codex", Disabled: true},
		},
		auths: map[string]string{
			"idx-oauth": `{"access_token":"oauth-token","email":"x@example.com"}`,
			"idx-api":   `{"api_key":"sk-secret","base_url":"https://example.test/v1"}`,
			"idx-off":   `{"access_token":"oauth-token"}`,
		},
	}
	s.SetAuthQuotaSource(src)

	accounts, err := s.AuthAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]AuthAccount{}
	for _, account := range accounts {
		byID[account.ID] = account
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %#v", accounts)
	}
	if got := byID["codex-aaa-x@example.com-pro.json"]; !got.OAuth || got.Provider != "codex" {
		t.Fatalf("oauth account = %#v", got)
	}
	if got := byID["openai-compatibility:agnes:4dab06a22e06"]; got.OAuth {
		t.Fatalf("api-key provider must not be reported as oauth: %#v", got)
	}
	if got := byID["openai-compatibility:agnes:4dab06a22e06"]; got.Provider != "openai-compatible-agnes" {
		t.Fatalf("compat provider normalised away: %#v", got)
	}
	if got := byID["codex-off@example.com-pro.json"]; !got.Disabled {
		t.Fatalf("disabled flag lost: %#v", got)
	}
	// The host identifier must be exposed verbatim: bindings match on it.
	if got := byID["openai-compatibility:agnes:4dab06a22e06"].ID; got != "openai-compatibility:agnes:4dab06a22e06" {
		t.Fatalf("id = %q", got)
	}
}

// A credential whose file cannot be read must not fail the whole list; it is
// reported as non-OAuth rather than guessed to be OAuth.
func TestAuthAccountsSurvivesUnreadableCredential(t *testing.T) {
	s := quotaService(t)
	src := &fakeQuotaSource{
		files: []AuthQuotaFile{
			{ID: "codex-unreadable@example.com-pro.json", AuthIndex: "idx-bad", Provider: "codex", Type: "codex"},
		},
		// Unparsable credential JSON: the per-index read fails, which must not
		// fail the list and must not be guessed to be OAuth.
		auth: `not-json`,
	}
	s.SetAuthQuotaSource(src)

	accounts, err := s.AuthAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].OAuth {
		t.Fatalf("unreadable credential = %#v", accounts)
	}
}
