package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// KeyAuthBinding restricts one plugin key to a set of host OAuth accounts.
// Accounts are addressed by (Provider, AuthID) so ids from different providers
// never collide. Priority is reserved for the phase-two fill-first strategy.
type KeyAuthBinding struct {
	PluginKeyID string
	Provider    string
	AuthID      string
	Priority    int
}

func normalizeKeyAuthBindings(keyID string, bindings []KeyAuthBinding) ([]KeyAuthBinding, error) {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return nil, fmt.Errorf("%w: plugin key id is required", ErrInvalidArgument)
	}
	out := make([]KeyAuthBinding, 0, len(bindings))
	seen := make(map[string]struct{}, len(bindings))
	for _, item := range bindings {
		provider, authID := normalizeBindingProvider(item.Provider), strings.TrimSpace(item.AuthID)
		if provider == "" || authID == "" {
			return nil, fmt.Errorf("%w: binding provider and auth id are required", ErrInvalidArgument)
		}
		identity := provider + "\x00" + authID
		if _, dup := seen[identity]; dup {
			continue
		}
		seen[identity] = struct{}{}
		out = append(out, KeyAuthBinding{PluginKeyID: keyID, Provider: provider, AuthID: authID, Priority: item.Priority})
	}
	return out, nil
}

func normalizeBindingProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	// Vendor aliasing applies to OAuth providers only. An API-key provider key is
	// "openai-compatible-<name>" and merely contains the substring "openai";
	// collapsing it into "codex" would file an API provider under an OAuth provider.
	if strings.HasPrefix(provider, "openai-compatible") {
		return provider
	}
	switch {
	case strings.Contains(provider, "codex") || strings.Contains(provider, "openai") || provider == "chatgpt":
		return "codex"
	case strings.Contains(provider, "claude") || strings.Contains(provider, "anthropic"):
		return "claude"
	case strings.Contains(provider, "antigravity") || strings.Contains(provider, "google") || provider == "gemini":
		return "antigravity"
	case strings.Contains(provider, "kimi") || strings.Contains(provider, "moonshot"):
		return "kimi"
	case provider == "xai" || strings.Contains(provider, "grok"):
		return "xai"
	default:
		return provider
	}
}

func replaceKeyAuthBindingsTx(ctx context.Context, tx *sql.Tx, keyID string, bindings []KeyAuthBinding) error {
	normalized, err := normalizeKeyAuthBindings(keyID, bindings)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_auth_bindings WHERE plugin_key_id = ?`, keyID); err != nil {
		return fmt.Errorf("clear key auth bindings: %w", err)
	}
	now := nowUnixMilli()
	for _, item := range normalized {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_auth_bindings(
			plugin_key_id, provider, auth_id, priority, created_at_unix_ms
		) VALUES (?, ?, ?, ?, ?)`, item.PluginKeyID, item.Provider, item.AuthID, item.Priority, now); err != nil {
			return fmt.Errorf("insert key auth binding: %w", err)
		}
	}
	return nil
}

// ReplaceKeyAuthBindings swaps every binding of one key inside a transaction.
func (s *Store) ReplaceKeyAuthBindings(ctx context.Context, keyID string, bindings []KeyAuthBinding) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace key auth bindings: %w", err)
	}
	defer tx.Rollback()
	if err := replaceKeyAuthBindingsTx(ctx, tx, keyID, bindings); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace key auth bindings: %w", err)
	}
	return nil
}

// ListKeyAuthBindingsByKeyIDs returns the bindings of many keys in one query,
// grouped by key id. It exists so management list endpoints do not issue one
// query per key. An empty id list returns an empty result without a query.
func (s *Store) ListKeyAuthBindingsByKeyIDs(ctx context.Context, keyIDs []string) (map[string][]KeyAuthBinding, error) {
	out := map[string][]KeyAuthBinding{}
	ids := make([]string, 0, len(keyIDs))
	seen := make(map[string]struct{}, len(keyIDs))
	for _, id := range keyIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_key_id, provider, auth_id, priority
		FROM key_auth_bindings WHERE plugin_key_id IN (`+sqlPlaceholders(len(ids))+`)
		ORDER BY plugin_key_id, provider, auth_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list key auth bindings by key ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item KeyAuthBinding
		if err := rows.Scan(&item.PluginKeyID, &item.Provider, &item.AuthID, &item.Priority); err != nil {
			return nil, fmt.Errorf("scan key auth binding: %w", err)
		}
		out[item.PluginKeyID] = append(out[item.PluginKeyID], item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate key auth bindings: %w", err)
	}
	return out, nil
}

// sqlPlaceholders renders "?, ?, ..." for n bound parameters.
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ListKeyAuthBindings returns the bindings of one key ordered by provider then auth id.
func (s *Store) ListKeyAuthBindings(ctx context.Context, keyID string) ([]KeyAuthBinding, error) {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_key_id, provider, auth_id, priority
		FROM key_auth_bindings WHERE plugin_key_id = ? ORDER BY provider, auth_id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("list key auth bindings: %w", err)
	}
	defer rows.Close()
	var out []KeyAuthBinding
	for rows.Next() {
		var item KeyAuthBinding
		if err := rows.Scan(&item.PluginKeyID, &item.Provider, &item.AuthID, &item.Priority); err != nil {
			return nil, fmt.Errorf("scan key auth binding: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate key auth bindings: %w", err)
	}
	return out, nil
}
