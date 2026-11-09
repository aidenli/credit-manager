package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/money"
)

// InsertUsageStatistics atomically appends display-only usage. IDs must be stable
// and nonempty: duplicate IDs are ignored (first write wins), including within a
// batch. Empty batches are a no-op. Model and nonnegative usage/reference cost
// are required; zero CreatedAt defaults to now. CostMicroUSD, ReservationID and
// Source are ignored and always stored as 0, "" and "statistics" respectively.
// Caller/key/pricing IDs are attribution only and need not exist. KeyLabel is
// resolved by display queries, not stored. No ledger, quota or reservation changes
// are made. Callers must avoid recording the same request in both reporting arms.
func (s *Store) InsertUsageStatistics(ctx context.Context, entries []UsageEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO usage_statistics (
		id, caller_id, plugin_key_id, executor_type, model, pricing_rule_id,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
		cache_creation_tokens, total_tokens, estimated_cost_micro_usd,
		tier, result, first_token_latency_ms, generation_duration_ms, tokens_per_second, thinking_intensity,
		auth_id, auth_index, auth_name, auth_label, auth_provider, auth_type, auth_email, auth_path,
		served_api, served_provider, created_at_unix_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("prepare usage statistics: %w", err)
	}
	defer stmt.Close()
	now := time.Now()
	for _, entry := range entries {
		entry.ID = strings.TrimSpace(entry.ID)
		entry.Model = strings.TrimSpace(entry.Model)
		if entry.ID == "" || entry.Model == "" || entry.EstimatedCostMicroUSD < 0 {
			return fmt.Errorf("%w: statistics id, model and non-negative reference cost are required", ErrInvalidArgument)
		}
		if err := entry.Usage.Validate(); err != nil {
			return fmt.Errorf("%w: statistics usage: %v", ErrInvalidArgument, err)
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = now
		}
		auth := entry.Auth
		_, err := stmt.ExecContext(ctx,
			entry.ID, entry.CallerID, entry.PluginKeyID, nullableString(entry.ExecutorType), entry.Model, entry.PricingRuleID,
			entry.Usage.Input, entry.Usage.Output, entry.Usage.Reasoning, entry.Usage.Cached, entry.Usage.CacheRead,
			entry.Usage.CacheCreation, money.ReportedTotal(entry.Usage), entry.EstimatedCostMicroUSD,
			nullableText(entry.Metrics.Tier), nullableText(entry.Metrics.Result), nullableDurationMillis(entry.Metrics.FirstTokenLatency),
			nullableDurationMillis(entry.Metrics.GenerationDuration), entry.Metrics.TokensPerSecond, nullableText(entry.Metrics.ThinkingIntensity),
			nullableString(auth.AuthID), nullableString(auth.AuthIndex), nullableString(auth.Name), nullableString(auth.Label),
			nullableString(auth.Provider), nullableString(auth.Type), nullableString(auth.Email), nullableString(auth.Path),
			boolColumn(entry.ServedAPI), strings.TrimSpace(entry.ServedProvider), entry.CreatedAt.UnixMilli())
		if err != nil {
			return fmt.Errorf("insert usage statistics: %w", err)
		}
	}
	return tx.Commit()
}
