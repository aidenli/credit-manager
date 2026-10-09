package plugin

import (
	"strings"
	"sync"
	"time"
)

// compatAPITargets remembers which models the host offered an OpenAI-compatible
// API provider for. The scheduler pick hook is the only place the plugin sees the
// candidate window, and the request that reaches the executor may be dispatched
// by the host to a different provider than the one the plugin picked, so the
// pick decision has to survive until that execution.
const (
	compatTargetTTL          = 30 * time.Minute
	maxCompatTargetEntries   = 256
	compatTargetSweepDivisor = 32
)

var (
	compatTargetMu    sync.RWMutex
	compatAPITarget   = map[string]time.Time{}
	compatPoisoned    = map[string]time.Time{}
	compatTargetCalls int
)

// compatPoisonTTL bounds how long one account-bound reasoning failure keeps the
// affected credential and model on the stripped-replay path. The client only
// heals when it stops replaying the items, so the mark has to outlive the request
// that discovered it without lasting forever.
const compatPoisonTTL = time.Hour

// markCompatPoisonedReasoning records that a replay of this credential and model
// carried reasoning state the selected account could not hydrate.
func markCompatPoisonedReasoning(keyFingerprint, model string) {
	fingerprint := strings.TrimSpace(keyFingerprint)
	model = strings.TrimSpace(model)
	if fingerprint == "" || model == "" {
		return
	}
	now := time.Now()
	compatTargetMu.Lock()
	defer compatTargetMu.Unlock()
	compatPoisoned[fingerprint+"\x00"+model] = now
	sweepCompatPoisonedLocked(now)
}

// compatReasoningPoisoned reports whether this credential and model already had
// an account-bound reasoning failure, in which case the replay is stripped before
// the upstream sees it rather than after another failure.
func compatReasoningPoisoned(keyFingerprint, model string) bool {
	fingerprint := strings.TrimSpace(keyFingerprint)
	model = strings.TrimSpace(model)
	if fingerprint == "" || model == "" {
		return false
	}
	compatTargetMu.RLock()
	defer compatTargetMu.RUnlock()
	seen, ok := compatPoisoned[fingerprint+"\x00"+model]
	return ok && time.Since(seen) <= compatPoisonTTL
}

// sweepCompatPoisonedLocked drops expired marks. Callers hold the write lock.
func sweepCompatPoisonedLocked(now time.Time) {
	for key, seen := range compatPoisoned {
		if now.Sub(seen) > compatPoisonTTL {
			delete(compatPoisoned, key)
		}
	}
	_ = now
}

// recordCompatAPITarget notes that a scheduling window for this model carried an
// OpenAI-compatible API provider. Both the requested model and the pricing model
// are recorded because either spelling can reach the executor.
func recordCompatAPITarget(models ...string) {
	now := time.Now()
	compatTargetMu.Lock()
	defer compatTargetMu.Unlock()
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			compatAPITarget[model] = now
		}
	}
	compatTargetCalls++
	if compatTargetCalls%compatTargetSweepDivisor == 0 {
		sweepCompatAPITargetsLocked(now)
	}
	if len(compatAPITarget) > maxCompatTargetEntries {
		sweepCompatAPITargetsLocked(now)
	}
}

// apiTargetForModels reports whether any of these model spellings was last seen
// with an API provider available.
func apiTargetForModels(models ...string) bool {
	now := time.Now()
	compatTargetMu.RLock()
	defer compatTargetMu.RUnlock()
	for _, model := range models {
		seen, ok := compatAPITarget[strings.TrimSpace(model)]
		if ok && now.Sub(seen) <= compatTargetTTL {
			return true
		}
	}
	return false
}

// sweepCompatAPITargetsLocked drops expired entries, and then the oldest ones if
// an unusually wide set of models still exceeds the bound. Callers hold the write
// lock.
func sweepCompatAPITargetsLocked(now time.Time) {
	for model, seen := range compatAPITarget {
		if now.Sub(seen) > compatTargetTTL {
			delete(compatAPITarget, model)
		}
	}
	for len(compatAPITarget) > maxCompatTargetEntries {
		oldest, oldestSeen := "", time.Time{}
		for model, seen := range compatAPITarget {
			if oldestSeen.IsZero() || seen.Before(oldestSeen) {
				oldest, oldestSeen = model, seen
			}
		}
		if oldest == "" {
			return
		}
		delete(compatAPITarget, oldest)
	}
}

// resetCompatAPITargets clears the memory. Tests use it to keep cases
// independent; the plugin never calls it in production.
func resetCompatAPITargets() {
	compatTargetMu.Lock()
	defer compatTargetMu.Unlock()
	compatAPITarget = map[string]time.Time{}
	compatPoisoned = map[string]time.Time{}
	compatTargetCalls = 0
}
