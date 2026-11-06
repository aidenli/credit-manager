package plugin

import (
	"strings"
	"sync"
	"time"
)

// In auth_only mode there is no reservation, so the Key concurrency cap is an
// in-memory count from request intercept to request.complete. A process restart
// starts from zero, which is the correct state after a crash.
const keySlotTTL = 30 * time.Minute

type keySlot struct {
	keyID string
	at    time.Time
}

var (
	keySlotsMu sync.Mutex
	keySlots   = map[string]keySlot{} // requestID -> slot
	keyActive  = map[string]int64{}   // keyID -> active count
)

func acquireKeySlot(requestID, keyID string, limit int64) bool {
	requestID, keyID = strings.TrimSpace(requestID), strings.TrimSpace(keyID)
	if requestID == "" || keyID == "" {
		return true
	}
	now := time.Now()
	keySlotsMu.Lock()
	defer keySlotsMu.Unlock()
	// shortcut: a lost request.complete leaks a slot for at most keySlotTTL.
	for id, slot := range keySlots {
		if now.Sub(slot.at) >= keySlotTTL {
			dropKeySlotLocked(id, slot)
		}
	}
	if _, held := keySlots[requestID]; held {
		return true
	}
	if limit > 0 && keyActive[keyID] >= limit {
		return false
	}
	keySlots[requestID] = keySlot{keyID: keyID, at: now}
	keyActive[keyID]++
	return true
}

func releaseKeySlot(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	keySlotsMu.Lock()
	if slot, ok := keySlots[requestID]; ok {
		dropKeySlotLocked(requestID, slot)
	}
	keySlotsMu.Unlock()
}

func dropKeySlotLocked(requestID string, slot keySlot) {
	delete(keySlots, requestID)
	if keyActive[slot.keyID] <= 1 {
		delete(keyActive, slot.keyID)
		return
	}
	keyActive[slot.keyID]--
}
