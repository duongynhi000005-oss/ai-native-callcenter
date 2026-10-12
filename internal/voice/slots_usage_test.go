// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageReadsWhatTheCounterHolds(t *testing.T) {
	t.Parallel()
	b, err := NewBotSlots(BotSlotsConfig{Cap: 10, InboundReserve: 2, Clock: newManualClock()})
	if err != nil {
		t.Fatal(err)
	}
	b.TryAcquire()
	b.TryAcquire()
	b.Reserve("a")
	if got := b.Usage(); got != (Usage{InUse: 2, Reserved: 1, Cap: 10, InboundReserve: 2}) {
		t.Errorf("usage = %+v", got)
	}
}

// The dialer sleeps until capacity appears, so every way capacity appears must
// say so: a session ending, a reservation released, a reservation expiring.
func TestEveryWayCapacityAppearsTellsTheListener(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	b, err := NewBotSlots(BotSlotsConfig{Cap: 5, ReservationTTL: time.Second, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	var told atomic.Int32
	b.SetOnFree(func() { told.Add(1) })

	b.TryAcquire()
	b.Free()
	if told.Load() != 1 {
		t.Errorf("told %d times after a session ended, want 1", told.Load())
	}
	b.Reserve("x")
	b.Release("x")
	if told.Load() != 2 {
		t.Errorf("told %d times after a release, want 2", told.Load())
	}
	b.Reserve("y")
	clock.advance(2 * time.Second)
	if told.Load() != 3 {
		t.Errorf("told %d times after an expiry, want 3", told.Load())
	}
	// Taking a slot is not capacity appearing.
	b.TryAcquire()
	b.Reserve("z")
	if told.Load() != 3 {
		t.Errorf("told %d times by acquisitions, want still 3", told.Load())
	}
}
