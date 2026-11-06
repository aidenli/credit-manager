package plugin

import "testing"

func TestKeySlotsEnforceLimitAndRelease(t *testing.T) {
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		releaseKeySlot(id)
	}
	if !acquireKeySlot("r1", "k", 2) || !acquireKeySlot("r2", "k", 2) {
		t.Fatal("first two requests must be admitted")
	}
	if acquireKeySlot("r3", "k", 2) {
		t.Fatal("third request must be rejected at limit 2")
	}
	if !acquireKeySlot("r4", "other", 2) {
		t.Fatal("other key must not share the count")
	}
	releaseKeySlot("r1")
	releaseKeySlot("r1") // double complete must not go negative
	if !acquireKeySlot("r3", "k", 2) {
		t.Fatal("slot must be free after release")
	}
	if acquireKeySlot("r5", "k", 2) {
		t.Fatal("double release must not free an extra slot")
	}
	if !acquireKeySlot("r6", "k", 0) {
		t.Fatal("limit 0 means unlimited")
	}
	for _, id := range []string{"r2", "r3", "r4", "r6"} {
		releaseKeySlot(id)
	}
}
