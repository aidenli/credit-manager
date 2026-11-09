package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

const usageStatisticsCapacity = 512
const usageStatisticsBatchSize = 64

// UsageStatistic contains only bounded scalar metadata; never a request context,
// body, response headers, source credential, or free-form upstream error.
type UsageStatistic struct {
	EventID, Principal, Model, Provider, ExecutorType         string
	AuthID, AuthIndex, AuthType, ServiceTier, ReasoningEffort string
	RequestedAt                                               time.Time
	Latency, TTFT                                             time.Duration
	Failed                                                    bool
	Usage                                                     money.TokenUsage
}

type UsageStatisticsStatus struct {
	Queued       int    `json:"queued"`
	Accepted     uint64 `json:"accepted"`
	Dropped      uint64 `json:"dropped"`
	WriteFailed  uint64 `json:"write_failed"`
	Unattributed uint64 `json:"unattributed"`
	Written      uint64 `json:"written"`
}

type usageStatisticsWriter struct {
	store                                                 *store.Store
	queue                                                 chan UsageStatistic
	done                                                  chan struct{}
	mu                                                    sync.Mutex
	closed                                                bool
	accepted, dropped, writeFailed, unattributed, written atomic.Uint64
}

func newUsageStatisticsWriter(st *store.Store) *usageStatisticsWriter {
	w := &usageStatisticsWriter{store: st, queue: make(chan UsageStatistic, usageStatisticsCapacity), done: make(chan struct{})}
	go w.run()
	return w
}

// ObserveUsageStatistic never waits for SQLite. Overflow is intentionally lossy
// and visible: this is telemetry, not a financial ledger.
func (s *Service) ObserveUsageStatistic(record UsageStatistic) bool {
	if s == nil || s.statistics == nil || !s.cfg.AuthOnly {
		return false
	}
	w := s.statistics
	for _, value := range []*string{&record.EventID, &record.Principal, &record.Model, &record.Provider, &record.ExecutorType, &record.AuthID, &record.AuthIndex, &record.AuthType, &record.ServiceTier, &record.ReasoningEffort} {
		if len(*value) > 512 {
			w.dropped.Add(1)
			return false
		}
		// Own only the bounded scalar, not a slice of a larger decoder buffer.
		*value = strings.Clone(strings.TrimSpace(*value))
	}
	for _, tokens := range []int64{record.Usage.Input, record.Usage.Output, record.Usage.Reasoning, record.Usage.Cached, record.Usage.CacheRead, record.Usage.CacheCreation, record.Usage.ReportedTotal, record.Usage.Images} {
		if tokens > math.MaxInt64/8 {
			w.dropped.Add(1)
			return false
		}
	}
	if record.Model == "" || record.Usage.Validate() != nil || record.Latency < 0 || record.TTFT < 0 {
		w.dropped.Add(1)
		return false
	}
	if record.Principal == "" {
		w.unattributed.Add(1)
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.dropped.Add(1)
		return false
	}
	select {
	case w.queue <- record:
		w.accepted.Add(1)
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

func (s *Service) DropUsageStatistic() {
	if s != nil && s.statistics != nil {
		s.statistics.dropped.Add(1)
	}
}

func (s *Service) UsageStatisticsStatus() UsageStatisticsStatus {
	if s == nil || s.statistics == nil {
		return UsageStatisticsStatus{}
	}
	w := s.statistics
	return UsageStatisticsStatus{Queued: len(w.queue), Accepted: w.accepted.Load(), Dropped: w.dropped.Load(), WriteFailed: w.writeFailed.Load(), Unattributed: w.unattributed.Load(), Written: w.written.Load()}
}

func (w *usageStatisticsWriter) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *usageStatisticsWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]UsageStatistic, 0, usageStatisticsBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.write(batch)
		clear(batch)
		batch = batch[:0]
	}
	for {
		select {
		case record, ok := <-w.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, record)
			if len(batch) == usageStatisticsBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (w *usageStatisticsWriter) write(batch []UsageStatistic) {
	// A detached deadline bounds best-effort work; no incoming context is retained.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries := make([]store.UsageEntry, 0, len(batch))
	for _, record := range batch {
		key, err := w.store.GetPluginKeyByPrincipal(ctx, record.Principal)
		if err != nil {
			if errors.Is(err, store.ErrPluginKeyNotFound) {
				w.unattributed.Add(1)
			} else {
				w.writeFailed.Add(1)
			}
			continue
		}
		id := uuid.NewString()
		if record.EventID != "" {
			// Namespace by plugin key: unrelated producers cannot deduplicate each other.
			sum := sha256.Sum256([]byte(key.ID + "\x00" + record.EventID))
			id = hex.EncodeToString(sum[:])
		}
		if record.RequestedAt.IsZero() {
			record.RequestedAt = time.Now().UTC()
		}
		result := "success"
		if record.Failed {
			result = "failed"
		}
		entry := store.UsageEntry{ID: "stat-" + id, CallerID: key.CallerID, PluginKeyID: key.ID,
			Model: record.Model, ExecutorType: record.ExecutorType, Usage: record.Usage, Source: "statistics", CreatedAt: record.RequestedAt,
			Auth:    store.AuthIdentity{AuthID: record.AuthID, AuthIndex: record.AuthIndex, Type: record.AuthType, Provider: record.Provider},
			Metrics: store.UsageMetrics{Result: &result},
		}
		serving := ServingFromHost(record.Provider, record.ExecutorType)
		entry.ServedAPI, entry.ServedProvider = serving.API, serving.Provider
		if record.ServiceTier != "" {
			entry.Metrics.Tier = &record.ServiceTier
		}
		if record.ReasoningEffort != "" {
			entry.Metrics.ThinkingIntensity = &record.ReasoningEffort
		}
		if record.TTFT > 0 {
			entry.Metrics.FirstTokenLatency = &record.TTFT
		}
		if record.Latency > 0 {
			entry.Metrics.GenerationDuration = &record.Latency
		}
		rule, err := w.store.ResolvePricingRule(ctx, record.Model)
		if err == nil && (!rule.Price.IsPerImage() || record.Usage.Images > 0) {
			cost, costErr := money.CostForTiered(record.Usage, rule.Price, rule.Tiers, record.Model, record.Provider, record.ServiceTier)
			if costErr == nil {
				entry.PricingRuleID = &rule.ID
				entry.EstimatedCostMicroUSD = cost
			}
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return
	}
	if err := w.store.InsertUsageStatistics(ctx, entries); err != nil {
		w.writeFailed.Add(uint64(len(entries)))
	} else {
		// Counts successfully processed events, including idempotent replays.
		w.written.Add(uint64(len(entries)))
	}
}
