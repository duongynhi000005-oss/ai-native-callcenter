// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

func TestACampaignDialsUpToItsConcurrencyAndRefillsAsCallsEnd(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Concurrency", func(s *camSpec) { s.max = 5 })
	r.importContacts(c, numbers("1", 40))
	r.start(c)

	if got := len(r.placer.liveCalls()); got != 5 {
		t.Fatalf("%d calls live after start, want maxConcurrent 5", got)
	}
	if p := r.progress(c); p.Activity != campaign.ActivityAtCapacity || p.InFlight != 5 {
		t.Errorf("progress = %+v, want AT_CAPACITY with 5 in flight", p)
	}
	live := r.placer.liveCalls()
	r.end(live[0], endAnswered)
	r.end(live[1], endAnswered)
	r.settle()
	if got := len(r.placer.liveCalls()); got != 5 {
		t.Errorf("%d live after two ended, want 5 again", got)
	}
	if r.placer.total() != 7 {
		t.Errorf("%d calls placed, want 7", r.placer.total())
	}
	r.noViolations()

	// What the placer was handed.
	first := r.placer.placedCalls()[0].Req
	if first.DIDNumber != "95099" || first.RingTimeout != 30*time.Second || first.CampaignID != c.ID ||
		first.UserData["name"] == "" || first.To == "" {
		t.Errorf("request = %+v", first)
	}
	a, err := r.st.Attempts().Get(context.Background(), first.CallID)
	if err != nil || a.ID != first.AttemptID || a.LegID != first.LegID || a.OriginateJobID == "" {
		t.Errorf("attempt = %+v, %v; want its ids on the request and the job recorded", a, err)
	}
}

func TestNothingIsAdmittedBeforeTheSwitchHasBeenReconciled(t *testing.T) {
	r := newRig(t, opts{held: true})
	c := r.newCampaign("Gate", nil)
	r.importContacts(c, numbers("2", 5))
	r.start(c)
	if r.placer.total() != 0 {
		t.Fatalf("%d calls placed before the gate opened", r.placer.total())
	}
	r.openGate()
	if r.placer.total() != 5 {
		t.Errorf("%d calls placed after the gate opened, want 5", r.placer.total())
	}
	r.noViolations()
}

func TestAnUnstartedPausedAndCancelledCampaignDialsNothing(t *testing.T) {
	r := newRig(t, opts{})
	draft := r.newCampaign("Draft", nil)
	r.importContacts(draft, numbers("3", 3))
	paused := r.newCampaign("Paused", nil)
	r.importContacts(paused, numbers("4", 3))
	r.start(paused)
	live := r.placer.liveCalls()
	if len(live) != 3 {
		t.Fatalf("setup: %d live", len(live))
	}
	if _, err := r.dialer().Pause(context.Background(), paused.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if got := r.campaignNow(paused); got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseManual {
		t.Fatalf("paused = %s/%s", got.Status, got.PauseReason)
	}
	// The calls in flight finish normally; nothing new starts.
	r.endAll(func(string) string { return endNoAnswer })
	r.tick(3 * time.Hour)
	if r.placer.total() != 3 {
		t.Errorf("%d calls placed, want the 3 from before the pause", r.placer.total())
	}
	for _, a := range r.attempts(paused) {
		if a.State != campaign.AttemptFinished || a.Outcome != campaign.OutcomeNoAnswer {
			t.Errorf("attempt %+v should have finished normally", a)
		}
	}

	// Resume: the retries come due.
	if _, err := r.dialer().Resume(context.Background(), paused.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if r.placer.total() != 6 {
		t.Errorf("%d calls placed after resume, want 6", r.placer.total())
	}

	// Cancel stops it for good, calls in flight finish.
	if _, err := r.dialer().Cancel(context.Background(), paused.ID); err != nil {
		t.Fatal(err)
	}
	r.endAll(func(string) string { return endNoAnswer })
	r.tick(48 * time.Hour)
	if r.placer.total() != 6 {
		t.Errorf("%d calls placed after cancel, want still 6", r.placer.total())
	}
	if got := r.campaignNow(paused); got.Status != campaign.StatusCancelled {
		t.Errorf("status = %s", got.Status)
	}
	r.noViolations()
}

func TestSynchronousDialErrorsFinishTheAttemptAsTheDesignSays(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Sync errors", func(s *camSpec) { s.max = 10 })
	cts := r.importContacts(c, numbers("5", 5))
	errs := map[string]error{
		cts[0].Number: outbound.ErrNumberOnDNC,
		cts[1].Number: outbound.ErrBadNumber,
		cts[2].Number: errors.New("bgapi send failed"),
		cts[3].Number: outbound.ErrNotReady,
	}
	r.placer.fail = func(req outbound.AIDialRequest) error {
		if e, ok := errs[req.To]; ok {
			delete(errs, req.To) // the not-ready one is tried again and works
			if req.To != cts[3].Number {
				errs[req.To] = e
			}
			return e
		}
		return nil
	}
	r.start(c)

	want := map[string]struct {
		outcome campaign.Outcome
		status  campaign.ContactStatus
	}{
		cts[0].Number: {campaign.OutcomeDNCSkipped, campaign.ContactDoNotCall},
		cts[1].Number: {campaign.OutcomeInvalidNumber, campaign.ContactInvalidNumber},
		cts[2].Number: {campaign.OutcomeOriginateFailed, campaign.ContactActive},
	}
	for number, w := range want {
		ct := r.contactOf(c, number)
		if ct.LastOutcome != w.outcome || ct.Status != w.status {
			t.Errorf("%s: outcome %s status %s, want %s %s", number, ct.LastOutcome, ct.Status, w.outcome, w.status)
		}
	}
	if ct := r.contactOf(c, cts[2].Number); ct.SystemFailures != 1 || ct.Attempts != 0 || ct.NextAttemptAt == nil {
		t.Errorf("an originate failure: %+v, want one system failure and a retry time", ct)
	}
	for _, a := range r.attempts(c) {
		if a.Number == cts[2].Number && a.HangupCause != "bgapi send failed" {
			t.Errorf("cause = %q, want the error text", a.HangupCause)
		}
	}
	// The not-ready one left no trace and was dialled on the next look.
	var forNotReady int
	for _, a := range r.attempts(c) {
		if a.Number == cts[3].Number {
			forNotReady++
		}
	}
	if forNotReady != 1 || r.placer.placedTo(cts[3].Number) != 1 {
		t.Errorf("not-ready contact: %d attempt rows, %d placements; want 1 each (the void left no row)",
			forNotReady, r.placer.placedTo(cts[3].Number))
	}
}

func TestACampaignCompletesWhenItsListIsExhaustedAndNotBefore(t *testing.T) {
	r := newRig(t, opts{})
	empty := r.newCampaign("Empty", nil)
	r.start(empty)
	if got := r.campaignNow(empty); got.Status != campaign.StatusRunning {
		t.Errorf("a campaign with no contacts is %s; it should wait for some", got.Status)
	}

	c := r.newCampaign("Completes", func(s *camSpec) { s.max = 10 })
	r.importContacts(c, numbers("6", 4))
	r.start(c)
	live := r.placer.liveCalls()
	for _, l := range live[:3] {
		r.end(l, endAnswered)
	}
	r.settle()
	if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
		t.Fatalf("status = %s with a call still in flight", got.Status)
	}
	r.end(live[3], endAnswered)
	r.settle()
	got := r.campaignNow(c)
	if got.Status != campaign.StatusCompleted || got.FinishedAt == nil {
		t.Errorf("status = %s finishedAt=%v, want COMPLETED", got.Status, got.FinishedAt)
	}
	if _, ok := r.dialer().Progress(c.ID); ok {
		t.Error("a completed campaign should have no activity")
	}
}

func TestActivityTellsWhyARunningCampaignIsNotDialing(t *testing.T) {
	r := newRig(t, opts{start: ny(t, "2026-06-01 06:00")})
	c := r.newCampaign("Activity", func(s *camSpec) { s.max = 2 })
	r.importContacts(c, numbers("7", 3))
	r.start(c)

	p := r.progress(c)
	if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
		t.Fatalf("status = %s; a closed window must not pause a campaign", got.Status)
	}
	if p.Activity != campaign.ActivityWaitingWindow || p.InFlight != 0 || p.NextDialAt == nil ||
		!p.NextDialAt.Equal(ny(t, "2026-06-01 09:00")) {
		t.Errorf("progress = %+v, want WAITING_WINDOW until 09:00", p)
	}
	if r.placer.total() != 0 {
		t.Errorf("%d calls placed outside the window", r.placer.total())
	}

	r.at(ny(t, "2026-06-01 09:00"))
	if p := r.progress(c); p.Activity != campaign.ActivityAtCapacity || p.InFlight != 2 {
		t.Errorf("at 09:00 progress = %+v, want AT_CAPACITY (2 of 3 placed)", p)
	}
	live := r.placer.liveCalls()
	r.end(live[0], endNoAnswer)
	r.end(live[1], endAnswered)
	r.settle()
	// The third is dialled; then only a retry, 2 hours away, remains.
	r.endAll(func(string) string { return endAnswered })
	p = r.progress(c)
	if p.Activity != campaign.ActivityWaitingRetry || p.NextDialAt == nil || !p.NextDialAt.Equal(ny(t, "2026-06-01 11:00")) {
		t.Errorf("progress = %+v, want WAITING_RETRY until 11:00", p)
	}
	r.at(ny(t, "2026-06-01 11:00"))
	if p := r.progress(c); p.Activity != campaign.ActivityDialing || p.InFlight != 1 {
		t.Errorf("at 11:00 progress = %+v, want DIALING", p)
	}
}

func TestACampaignThatIsNotRunningHasNoActivity(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Draft", nil)
	if _, ok := r.dialer().Progress(c.ID); ok {
		t.Error("a draft has no activity")
	}
	r.importContacts(c, numbers("8", 2))
	r.start(c)
	if _, err := r.dialer().Pause(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if _, ok := r.dialer().Progress(c.ID); ok {
		t.Error("a paused campaign has no activity")
	}
}

func TestTheTotalRingingCapAndTheBotRuleBoundAdmission(t *testing.T) {
	t.Run("ringing cap", func(t *testing.T) {
		r := newRig(t, opts{maxRinging: 4})
		c := r.newCampaign("Ringing", func(s *camSpec) { s.max = 50 })
		r.importContacts(c, numbers("9", 30))
		r.start(c)
		if got := len(r.placer.liveCalls()); got != 4 {
			t.Fatalf("%d ringing, want the cap 4", got)
		}
		// An answered call stops ringing: another may be placed.
		r.answer(r.placer.liveCalls()[0])
		if got := len(r.placer.liveCalls()); got != 5 {
			t.Errorf("%d live after one answered, want 5", got)
		}
	})
	t.Run("bot capacity", func(t *testing.T) {
		slots := &fakeSlots{u: voice.Usage{Cap: 10, InboundReserve: 2, InUse: 3}}
		r := newRig(t, opts{slots: slots})
		c := r.newCampaign("Bot", func(s *camSpec) { s.max = 50 })
		r.importContacts(c, numbers("10", 30))
		r.start(c)
		// 10 - 2 reserved for inbound - 3 in use: five more may ring.
		if got := len(r.placer.liveCalls()); got != 5 {
			t.Fatalf("%d live, want 5", got)
		}
		// Capacity appears and the dialer is told.
		slots.set(voice.Usage{Cap: 10, InboundReserve: 2, InUse: 1})
		r.dialer().SlotFreed()
		r.settle()
		if got := len(r.placer.liveCalls()); got != 7 {
			t.Errorf("%d live after two slots freed, want 7", got)
		}
		// Inbound takes everything outside the reserve: nothing new is admitted.
		slots.set(voice.Usage{Cap: 10, InboundReserve: 2, InUse: 8})
		r.dialer().Wake()
		r.settle()
		if got := len(r.placer.liveCalls()); got != 7 {
			t.Errorf("%d live with the bot full, want still 7", got)
		}
	})
}

func TestTwoCampaignsShareOneCapBetweenThem(t *testing.T) {
	slots := &fakeSlots{u: voice.Usage{Cap: 10}}
	r := newRig(t, opts{slots: slots})
	a := r.newCampaign("A", func(s *camSpec) { s.max = 20 })
	b := r.newCampaign("B", func(s *camSpec) { s.max = 20 })
	r.importContacts(a, numbers("11", 40))
	r.importContacts(b, numbers("12", 40))
	r.start(a)
	r.start(b)

	count := func() (na, nb int) {
		for _, l := range r.placer.liveCalls() {
			if l.Req.CampaignID == a.ID {
				na++
			} else {
				nb++
			}
		}
		return
	}
	// A started first and took the whole cap; B waits for room.
	if na, nb := count(); na+nb != 10 {
		t.Fatalf("A has %d and B %d live; the cap is 10", na, nb)
	}
	// As calls end, the room is shared between them in turn.
	r.endAll(func(string) string { return endAnswered })
	na, nb := count()
	if na+nb != 10 || na != 5 || nb != 5 {
		t.Fatalf("after the first calls ended A has %d and B %d live; want 5 each", na, nb)
	}
	for i := 0; i < 5; i++ {
		r.endAll(func(string) string { return endAnswered })
		na, nb = count()
		if na+nb > 10 {
			t.Fatalf("round %d: %d live, over the cap", i, na+nb)
		}
	}
	r.noViolations()
}

func TestAMaxConcurrentChangeAffectsOnlyLaterAdmissions(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Patch", func(s *camSpec) { s.max = 3 })
	r.importContacts(c, numbers("13", 30))
	r.start(c)
	if len(r.placer.liveCalls()) != 3 {
		t.Fatal("setup")
	}
	svc := campaign.NewService(r.st.Campaigns(), r.dialer(), campaign.Env{OutboundHours: r.cfg.OutboundHours}).
		WithDialer(r.dialer(), r.dialer())
	five := 5
	if _, err := svc.Update(context.Background(), c.ID, campaign.Patch{MaxConcurrent: &five}); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if got := len(r.placer.liveCalls()); got != 5 {
		t.Errorf("%d live after raising maxConcurrent to 5 (the 3 already ringing untouched), want 5", got)
	}
	two := 2
	if _, err := svc.Update(context.Background(), c.ID, campaign.Patch{MaxConcurrent: &two}); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if got := len(r.placer.liveCalls()); got != 5 {
		t.Errorf("lowering the limit hung calls up: %d live", got)
	}
	r.endAll(func(string) string { return endAnswered })
	if got := len(r.placer.liveCalls()); got != 2 {
		t.Errorf("%d live after they ended, want the new limit 2", got)
	}
}

func TestCallsCarryTheirContactsSlotsAndTheCampaignsRingTimeout(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Fields", func(s *camSpec) { s.ring = 20; s.max = 1 })
	r.importContacts(c, []contactSpec{{number: "+8613999000001"}})
	r.start(c)
	req := r.placer.placedCalls()[0].Req
	if req.RingTimeout != 20*time.Second || req.UserData["name"] != "c0" || req.To != "+8613999000001" {
		t.Errorf("request = %+v", req)
	}
	_ = fmt.Sprint
	_ = uuid.Nil
}

// The change hook hears about a campaign when its status, its activity, its
// in-flight count or its counters move, and about the one that completes.
func TestTheChangeHookHearsOfEveryMovementOfACampaign(t *testing.T) {
	var mu sync.Mutex
	touched := map[uuid.UUID]int{}
	r := newRig(t, opts{onChange: func(id uuid.UUID) { mu.Lock(); touched[id]++; mu.Unlock() }})
	count := func(id uuid.UUID) int { mu.Lock(); defer mu.Unlock(); return touched[id] }

	c := r.newCampaign("Hook", func(s *camSpec) { s.max = 10 })
	r.importContacts(c, numbers("6", 2))
	r.start(c)
	afterStart := count(c.ID)
	if afterStart == 0 {
		t.Fatal("starting a campaign told nobody")
	}
	live := r.placer.liveCalls()
	r.end(live[0], endAnswered)
	r.settle()
	afterFinish := count(c.ID)
	if afterFinish <= afterStart {
		t.Error("an attempt finishing told nobody")
	}
	r.end(live[1], endAnswered)
	r.settle()
	if got := r.campaignNow(c); got.Status != campaign.StatusCompleted || count(c.ID) <= afterFinish {
		t.Errorf("status %s touches %d: completion told nobody", got.Status, count(c.ID))
	}
}
