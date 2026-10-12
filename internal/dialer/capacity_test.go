// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

func constRate(p float64) func(time.Duration) float64 {
	return func(time.Duration) float64 { return p }
}

// With no history every ringing leg is planned to answer, so a dialer whose
// every call is answered never meets a full bot.
func TestColdStartPlansEveryRingingLegAsAnAnswerAndOverflowsNothing(t *testing.T) {
	s := newSim(t, simSpec{capacity: 60, inboundReserve: 6, campaigns: 1, rates: []func(time.Duration) float64{constRate(1)},
		seed: 1, maxConcurrent: 1000})
	s.run(20 * time.Minute)
	c := s.store.camps[0]
	if s.store.count(c.ID, campaign.OutcomeOverflow) != 0 {
		t.Fatalf("%d overflows with a dialer that planned for every answer", s.store.count(c.ID, campaign.OutcomeOverflow))
	}
	if s.placements < 100 {
		t.Fatalf("only %d calls placed", s.placements)
	}
	if s.peakInUse > 90 {
		t.Errorf("peak bot sessions %d, above cap 100 minus reserve 10", s.peakInUse)
	}
}

func TestAtAKnownTwentyPercentAnswerRateTheBotIsFilledAndOverflowStaysLow(t *testing.T) {
	run := func(noOverdial bool) (util, overflow float64, attempts int, peak int) {
		s := newSim(t, simSpec{capacity: 60, inboundReserve: 6, campaigns: 1,
			rates: []func(time.Duration) float64{constRate(0.2)}, seed: 7, maxConcurrent: 1000, noOverdial: noOverdial})
		s.run(8 * time.Minute) // warm-up: history builds, the bot fills
		s.measureFrom()
		s.run(40 * time.Minute)
		c := s.store.camps[0]
		return s.utilisation(s.start.Add(40 * time.Minute)), s.overflowRate(c.ID), s.placements, s.peakInUse
	}
	base, baseOv, baseN, _ := run(true)
	util, ov, n, peak := run(false)
	t.Logf("no over-dial: utilisation %.3f overflow %.4f attempts %d", base, baseOv, baseN)
	t.Logf("expected-answer: utilisation %.3f overflow %.4f attempts %d peak %d", util, ov, n, peak)
	if n < 3000 {
		t.Fatalf("only %d attempts", n)
	}
	if util < base*1.5 {
		t.Errorf("utilisation %.3f is not well above the no-over-dial %.3f", util, base)
	}
	if ov > 0.01 {
		t.Errorf("overflow %.4f above 1%%", ov)
	}
	if baseOv != 0 {
		t.Errorf("the conservative rule overflowed: %.4f", baseOv)
	}
}

// Held back by nothing but the bot: inbound holds everything outside the
// reserve, campaigns admit nothing, and capacity inbound still has is untouched.
func TestInboundHoldingEverythingOutsideTheReserveStopsCampaigns(t *testing.T) {
	s := newSim(t, simSpec{capacity: 50, inboundReserve: 5, campaigns: 1, seed: 3, maxConcurrent: 100})
	s.inbound = 45
	s.run(10 * time.Minute)
	if s.placements != 0 {
		t.Fatalf("%d calls placed while inbound held everything outside the reserve", s.placements)
	}
	if p, _ := s.d.Progress(s.store.camps[0].ID); p.Activity != campaign.ActivityAtCapacity {
		t.Errorf("activity = %v, want AT_CAPACITY", p.Activity)
	}
	if s.peakInUse > 50 {
		t.Errorf("peak %d above cap", s.peakInUse)
	}
}

// A campaign restarts on a history that no longer describes its list: the last
// two hundred rung attempts say 2 % answered, and the list now answers 30 %.
// The estimate lets far too many legs ring, the answers overflow, and overflow
// control falls back to planning for every ringing leg to answer. Once real
// samples have replaced the stale history and the overflow rate is under the
// tighten rate again, the estimate is back.
func TestOverflowAboveTheStopRateFallsBackAndRecovers(t *testing.T) {
	stale := func(uuid.UUID) []campaign.Outcome {
		h := make([]campaign.Outcome, 200)
		for i := range h {
			h[i] = campaign.OutcomeNoAnswer
			if i%50 == 0 {
				h[i] = campaign.OutcomeAnswered // 4 in 200
			}
		}
		return h
	}
	s := newSim(t, simSpec{capacity: 60, inboundReserve: 6, campaigns: 1, primeHistory: stale,
		rates: []func(time.Duration) float64{constRate(0.3)}, seed: 11, maxConcurrent: 1000})
	c := s.store.camps[0]

	s.run(4 * time.Minute)
	st := s.d.stats[c.ID]
	t.Logf("4 minutes in: %d overflows, overflow rate %.3f, mode %v, bound %.3f",
		s.store.count(c.ID, campaign.OutcomeOverflow), st.OverflowRate(), st.Mode(), st.Bound())
	if s.store.count(c.ID, campaign.OutcomeOverflow) == 0 {
		t.Fatal("the stale history caused no overflow: the scenario proves nothing")
	}
	if st.Mode() != campaign.OverflowStopped || st.Bound() != 1 {
		t.Errorf("mode %v bound %.3f, want stopped and 1 (planning for every answer)", st.Mode(), st.Bound())
	}

	s.run(40 * time.Minute)
	t.Logf("40 minutes in: overflow rate %.4f, mode %v, bound %.3f", st.OverflowRate(), st.Mode(), st.Bound())
	if st.Mode() == campaign.OverflowStopped {
		t.Error("never recovered from the fall-back")
	}
	if st.Bound() >= 0.6 {
		t.Errorf("bound %.3f after recovery, want the estimate of a 30%% list back", st.Bound())
	}
	// And it stays quiet: the next quarter hour overflows at most the tighten rate.
	ov, ans := s.store.count(c.ID, campaign.OutcomeOverflow), s.store.count(c.ID, campaign.OutcomeAnswered)
	s.run(55 * time.Minute)
	dov := s.store.count(c.ID, campaign.OutcomeOverflow) - ov
	dans := s.store.count(c.ID, campaign.OutcomeAnswered) - ans + dov
	if dans == 0 || float64(dov)/float64(dans) > 0.01 {
		t.Errorf("overflow %d of %d answered in the last quarter hour, want at most 1%%", dov, dans)
	}
}

func TestTwoCampaignsWithDifferentRatesShareTheBotWithoutOverflow(t *testing.T) {
	s := newSim(t, simSpec{capacity: 60, inboundReserve: 6, campaigns: 2,
		rates: []func(time.Duration) float64{constRate(0.1), constRate(0.5)}, seed: 5, maxConcurrent: 1000})
	s.run(8 * time.Minute)
	s.measureFrom()
	s.run(40 * time.Minute)
	for _, c := range s.store.camps {
		if r := s.overflowRate(c.ID); r > 0.015 {
			t.Errorf("campaign %s overflow %.4f", c.Name, r)
		}
		t.Logf("campaign %s attempts %d overflow %.4f", c.Name, len(s.store.history[c.ID]), s.overflowRate(c.ID))
	}
	t.Logf("utilisation %.3f", s.utilisation(s.start.Add(40*time.Minute)))
}

// Campaigns sharing a DID share its limit: ringing and talking never exceed it.
func TestADIDLimitBoundsRingingPlusTalkingAcrossCampaigns(t *testing.T) {
	s := newSim(t, simSpec{capacity: 220, inboundReserve: 22, campaigns: 2,
		rates: []func(time.Duration) float64{constRate(0.3), constRate(0.3)}, seed: 9, maxConcurrent: 1000,
		didLimit: map[string]int{"95099": 3}})
	s.run(20 * time.Minute)
	if s.lines.peak["95099"] > 3 {
		t.Fatalf("peak lines %d on a DID limited to 3", s.lines.peak["95099"])
	}
	if s.lines.peak["95099"] != 3 {
		t.Errorf("the DID never filled: peak %d", s.lines.peak["95099"])
	}
	a, b := len(s.store.history[s.store.camps[0].ID]), len(s.store.history[s.store.camps[1].ID])
	if a == 0 || b == 0 || a > 3*b || b > 3*a {
		t.Errorf("the two campaigns did not share the DID: %d and %d attempts", a, b)
	}
	if p, _ := s.d.Progress(s.store.camps[0].ID); p.Activity != campaign.ActivityAtCapacity {
		t.Errorf("activity = %v, want AT_CAPACITY", p.Activity)
	}
}

// A line limit error from the placer (a hand-dialled call took the last line in
// the same instant) voids the attempt: nothing counted, no outcome.
func TestASynchronousLineLimitErrorVoidsTheAttempt(t *testing.T) {
	s := newSim(t, simSpec{capacity: 50, inboundReserve: 5, campaigns: 1, seed: 2, maxConcurrent: 3})
	first := true
	s.failPlace = func(outbound.AIDialRequest) error {
		if first {
			first = false
			return outbound.ErrDIDChannelsExhausted
		}
		return nil
	}
	s.run(time.Minute)
	if s.store.voided != 1 {
		t.Fatalf("%d attempts voided, want 1", s.store.voided)
	}
	if got := len(s.store.history[s.store.camps[0].ID]); got > s.placements {
		t.Errorf("a voided attempt left an outcome: %d outcomes for %d placements", got, s.placements)
	}
	if s.placements < 3 {
		t.Errorf("the campaign did not go on dialing after the void: %d placed", s.placements)
	}
}

// The answer rate moves while the campaign runs: the estimate follows it, in
// both directions, without a burst of overflow.
func TestAnAnswerRateThatShiftsMidRunIsFollowedWithoutOverflow(t *testing.T) {
	shift := func(since time.Duration) float64 {
		switch {
		case since < 15*time.Minute:
			return 0.1
		case since < 30*time.Minute:
			return 0.35
		}
		return 0.15
	}
	s := newSim(t, simSpec{capacity: 60, inboundReserve: 6, campaigns: 1,
		rates: []func(time.Duration) float64{shift}, seed: 21, maxConcurrent: 1000})
	c := s.store.camps[0]
	for _, until := range []time.Duration{15, 30, 45} {
		s.run(until * time.Minute)
		st := s.d.stats[c.ID]
		t.Logf("%3d min: bound %.3f overflow total %d mode %v", until, st.Bound(),
			s.store.count(c.ID, campaign.OutcomeOverflow), st.Mode())
	}
	if r := s.overflowRate(c.ID); r > 0.02 {
		t.Errorf("overflow %.4f over the whole run, want at most 2%% through two shifts", r)
	}
}

// fakeLines is a DID line account whose figures a test sets.
type fakeLines struct {
	mu     sync.Mutex
	limits map[string]int
	inUse  map[string]int
}

func (f *fakeLines) DIDLimits(context.Context) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.limits {
		out[k] = v
	}
	return out, nil
}

func (f *fakeLines) LinesInUse(did string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inUse[did]
}

func (f *fakeLines) add(did string, n int) {
	f.mu.Lock()
	f.inUse[did] += n
	f.mu.Unlock()
}

func (f *fakeLines) set(did string, n int) {
	f.mu.Lock()
	f.inUse[did] = n
	f.mu.Unlock()
}

// A DID at its line limit holds admission: no attempt is written, the activity
// says so, and the campaign goes on when a line comes back.
func TestADIDAtItsLimitHoldsAdmissionWithoutAnAttempt(t *testing.T) {
	lines := &fakeLines{limits: map[string]int{"95099": 2}, inUse: map[string]int{"95099": 2}}
	r := newRig(t, opts{lines: lines})
	// Placing a call takes a line, as the outbound service does.
	r.placer.fail = func(req outbound.AIDialRequest) error {
		lines.add(req.DIDNumber, 1)
		return nil
	}
	c := r.newCampaign("Lines", nil)
	r.importContacts(c, numbers("7", 10))
	r.start(c)

	if r.placer.total() != 0 || len(r.attempts(c)) != 0 {
		t.Fatalf("%d placed and %d attempts written against a DID with no line free", r.placer.total(), len(r.attempts(c)))
	}
	if p := r.progress(c); p.Activity != campaign.ActivityAtCapacity {
		t.Errorf("activity = %v, want AT_CAPACITY", p.Activity)
	}
	lines.set("95099", 1) // another call on the DID ended
	r.dialer().Wake()
	r.settle()
	if got := r.placer.total(); got != 1 {
		t.Errorf("%d placed with one line free, want exactly 1", got)
	}
}

// A hand-dialled call took the last line between the check and the originate:
// the placer says so, the attempt is voided, and the contact is still due.
func TestALineLimitErrorFromThePlacerVoidsTheAttemptInTheStore(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Void", func(s *camSpec) { s.max = 1 })
	cs := r.importContacts(c, numbers("8", 1))
	refused := true
	r.placer.fail = func(outbound.AIDialRequest) error {
		if refused {
			refused = false
			return outbound.ErrDIDChannelsExhausted
		}
		return nil
	}
	r.start(c)
	r.settle()

	if got := r.placer.total(); got != 1 {
		t.Fatalf("%d placed: the voided contact should have been dialled again at once", got)
	}
	atts := r.attempts(c)
	if len(atts) != 1 || atts[0].Outcome != "" {
		t.Fatalf("attempts = %+v, want the one live attempt and no voided one", atts)
	}
	if ct := r.contactOf(c, cs[0].Number); ct.Attempts != 0 || ct.SystemFailures != 0 {
		t.Errorf("a voided attempt was counted: %+v", ct)
	}
}

// An answered leg waiting in the overflow hold counts in the bot rule, and so
// does the inbound reserve, whichever of the counter's and the dialer's is larger.
func TestAnsweredLegsWaitingForASlotAndTheReserveBoundAdmission(t *testing.T) {
	tests := []struct {
		name         string
		usage        voice.Usage
		cfgReserve   int
		wantRinging  int
		descriptions string
	}{
		{"nothing waiting", voice.Usage{InUse: 8, Cap: 20}, 0, 12, "20 - 8 free"},
		{"two waiting", voice.Usage{InUse: 8, Waiting: 2, Cap: 20}, 0, 10, "the waiting legs hold the slots they are about to take"},
		{"reserved slots count", voice.Usage{InUse: 4, Reserved: 4, Cap: 20}, 0, 12, ""},
		{"the counter's reserve", voice.Usage{InUse: 8, Cap: 20, InboundReserve: 4}, 0, 8, ""},
		{"the dialer's reserve", voice.Usage{InUse: 8, Cap: 20}, 6, 6, ""},
		{"the larger of the two", voice.Usage{InUse: 8, Cap: 20, InboundReserve: 2}, 6, 6, ""},
		{"inbound holds all outside the reserve", voice.Usage{InUse: 14, Cap: 20}, 6, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slots := &fakeSlots{}
			slots.set(tt.usage)
			r := newRig(t, opts{slots: slots})
			r.cfg.InboundReserve = tt.cfgReserve
			r.restart()
			c := r.newCampaign("Waiting", func(s *camSpec) { s.max = 50 })
			r.importContacts(c, numbers("9", 50))
			r.start(c)
			if got := len(r.placer.liveCalls()); got != tt.wantRinging {
				t.Fatalf("%d ringing, want %d", got, tt.wantRinging)
			}
			r.noViolations()
		})
	}
}

// A campaign that reaches AT_CAPACITY, and one that leaves it, is reported to
// the change hook (what becomes CAMPAIGN_UPDATED).
func TestReachingAndLeavingAtCapacityFiresTheChangeHook(t *testing.T) {
	var mu sync.Mutex
	var seen int
	s := newSim(t, simSpec{capacity: 50, inboundReserve: 5, campaigns: 1, seed: 3, maxConcurrent: 100,
		onChange: func(uuid.UUID) { mu.Lock(); seen++; mu.Unlock() }})
	s.inbound = 45
	s.run(time.Minute)
	mu.Lock()
	atCapacity := seen
	mu.Unlock()
	if p, _ := s.d.Progress(s.store.camps[0].ID); p.Activity != campaign.ActivityAtCapacity || atCapacity == 0 {
		t.Fatalf("activity %v with %d changes reported, want AT_CAPACITY and a report", p.Activity, atCapacity)
	}
	s.mu.Lock()
	s.inbound = 0
	s.mu.Unlock()
	s.d.Wake()
	s.run(2 * time.Minute)
	mu.Lock()
	defer mu.Unlock()
	if seen <= atCapacity {
		t.Errorf("leaving AT_CAPACITY was not reported: %d then %d", atCapacity, seen)
	}
}
