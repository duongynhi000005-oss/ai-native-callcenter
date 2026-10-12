// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// importArmed adds contacts to a running campaign: the import arms them with
// the database's clock, the test moves that to the fake clock's.
func (r *rig) importArmed(c campaign.Campaign, specs []contactSpec) {
	r.t.Helper()
	r.importContacts(c, specs)
	r.rearm(c)
	r.dialer().Wake()
	r.settle()
}

func TestAWindowCrossingMidnightOpensAndClosesOnTheWallClock(t *testing.T) {
	// Outer hours 20:00-04:00 and a campaign window 22:00-02:00 that belongs
	// to the day it starts on.
	r := newRig(t, opts{outer: "20:00-04:00", start: ny(t, "2026-06-01 21:58")})
	c := r.newCampaign("Midnight", func(s *camSpec) {
		s.max = 100
		s.window = campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "22:00", End: "02:00"}}}
	})
	r.importContacts(c, numbers("20", 2))
	r.start(c)
	if r.placer.total() != 0 || r.progress(c).Activity != campaign.ActivityWaitingWindow {
		t.Fatalf("placed %d at 21:58, activity %s; want none and WAITING_WINDOW", r.placer.total(), r.progress(c).Activity)
	}
	r.at(ny(t, "2026-06-01 21:59"))
	if r.placer.total() != 0 {
		t.Fatal("dialled a minute before the window opened")
	}
	r.at(ny(t, "2026-06-01 22:00"))
	if r.placer.total() != 2 {
		t.Fatalf("%d placed when the window opened, want 2", r.placer.total())
	}
	r.endAll(keepRinging(numbers("20", 1)[0].number, endAnswered))

	r.at(ny(t, "2026-06-02 00:30"))
	r.importArmed(c, numbers("21", 2))
	if r.placer.total() != 4 {
		t.Errorf("%d placed after midnight, inside the window, want 4", r.placer.total())
	}
	r.endAll(keepRinging(numbers("20", 1)[0].number, endAnswered))

	r.at(ny(t, "2026-06-02 01:59"))
	r.importArmed(c, numbers("22", 1))
	if r.placer.total() != 5 {
		t.Errorf("%d placed at 01:59, want 5", r.placer.total())
	}
	r.endAll(keepRinging(numbers("20", 1)[0].number, endAnswered))

	r.at(ny(t, "2026-06-02 02:00"))
	r.importArmed(c, numbers("23", 2))
	if r.placer.total() != 5 {
		t.Errorf("%d placed at 02:00, want still 5: the window is over", r.placer.total())
	}
	if p := r.progress(c); p.Activity != campaign.ActivityDialing || p.NextDialAt == nil || !p.NextDialAt.Equal(ny(t, "2026-06-02 22:00")) {
		t.Errorf("progress = %+v, want the anchor call DIALING and the next dial at 22:00", p)
	}
	r.at(ny(t, "2026-06-02 22:00"))
	if r.placer.total() != 7 {
		t.Errorf("%d placed the next evening, want 7", r.placer.total())
	}
}

func TestACallInFlightWhenTheWindowClosesCompletesAndNothingNewStarts(t *testing.T) {
	r := newRig(t, opts{start: ny(t, "2026-06-01 17:58")})
	c := r.newCampaign("Closing", func(s *camSpec) { s.max = 3 })
	r.importContacts(c, numbers("24", 10))
	r.start(c)
	if len(r.placer.liveCalls()) != 3 {
		t.Fatalf("%d live at 17:58, want 3", len(r.placer.liveCalls()))
	}
	r.at(ny(t, "2026-06-01 18:00"))
	if len(r.placer.liveCalls()) != 3 {
		t.Fatal("a closing window hung up calls in flight")
	}
	r.at(ny(t, "2026-06-01 18:01"))
	// The calls end after the window; nothing replaces them.
	live := r.placer.liveCalls()
	for _, l := range live {
		r.end(l, endNoAnswer)
	}
	r.settle()
	if r.placer.total() != 3 {
		t.Errorf("%d placed in all, want 3: no new call after 18:00", r.placer.total())
	}
	// The retry (two hours on, 20:01) falls outside the window and goes to the
	// next window's start; so does everyone who was still waiting.
	want := ny(t, "2026-06-02 09:00")
	for _, ct := range r.contacts(c) {
		if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(want) {
			t.Fatalf("contact %s due %v, want %v", ct.Number, ct.NextAttemptAt, want)
		}
	}
	if r.progress(c).Activity != campaign.ActivityWaitingWindow {
		t.Errorf("activity = %s", r.progress(c).Activity)
	}
	r.at(want)
	if len(r.placer.liveCalls()) != 3 {
		t.Errorf("%d live at 09:00 the next day, want 3", len(r.placer.liveCalls()))
	}
	r.noViolations()
}

func TestEachContactIsDialledInItsOwnZoneAcrossADaylightSavingChange(t *testing.T) {
	// 2026-03-08: the US clocks go forward at 02:00. At 13:00 UTC it is 09:00
	// in New York (EDT), 06:00 in Los Angeles and 21:00 in Shanghai.
	utc := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	r := newRig(t, opts{start: utc("2026-03-08 12:59")})
	c := r.newCampaign("Zones", func(s *camSpec) { s.max = 100; s.zone = "Asia/Shanghai" })
	var specs []contactSpec
	for i := 0; i < 3; i++ {
		specs = append(specs,
			contactSpec{number: fmt.Sprintf("+1212555010%d", i), zone: "America/New_York"},
			contactSpec{number: fmt.Sprintf("+1310555020%d", i), zone: "America/Los_Angeles"},
			contactSpec{number: fmt.Sprintf("+8613800000%03d", i)}, // the campaign's zone: Shanghai
		)
	}
	r.importContacts(c, specs)
	r.start(c)
	zoneOf := func() map[string]int {
		m := map[string]int{}
		for _, l := range r.placer.liveCalls() {
			switch l.Req.To[:2] {
			case "+1":
				if l.Req.To[2] == '2' {
					m["NY"]++
				} else {
					m["LA"]++
				}
			default:
				m["SH"]++
			}
		}
		return m
	}
	if r.placer.total() != 0 {
		t.Fatalf("%v dialled at 12:59 UTC, when nobody's window is open (New York is at 08:59)", zoneOf())
	}
	r.at(utc("2026-03-08 13:00"))
	if m := zoneOf(); m["NY"] != 3 || m["LA"] != 0 || m["SH"] != 0 {
		t.Errorf("at 13:00 UTC: %v, want only New York (09:00 EDT, an hour earlier than EST would be)", m)
	}
	r.at(utc("2026-03-08 16:00"))
	if m := zoneOf(); m["NY"] != 3 || m["LA"] != 3 || m["SH"] != 0 {
		t.Errorf("at 16:00 UTC: %v, want Los Angeles (09:00 PDT) too", m)
	}
	r.at(utc("2026-03-09 01:00"))
	if m := zoneOf(); m["SH"] != 3 {
		t.Errorf("at 01:00 UTC the next day: %v, want Shanghai (09:00) too", m)
	}
	r.noViolations()
}

func TestARetryThatLandsOutsideTheWindowGoesToTheNextWindowStartOnDSTDay(t *testing.T) {
	r := newRig(t, opts{start: ny(t, "2026-03-07 17:30")})
	c := r.newCampaign("Retry", func(s *camSpec) { s.max = 5 })
	r.importContacts(c, numbers("25", 1))
	r.start(c)
	r.endAll(func(string) string { return endNoAnswer }) // retry in 2 h: 19:30, outside 09:00-18:00
	ct := r.contactOf(c, numbers("25", 1)[0].number)
	// The next morning is the day the clocks go forward: 09:00 EDT is 13:00 UTC.
	want := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(want) {
		t.Fatalf("retry due %v, want %v (09:00 on the day of the change)", ct.NextAttemptAt, want)
	}
	r.at(want.Add(-time.Minute))
	if r.placer.total() != 1 {
		t.Fatal("redialled before the window opened")
	}
	r.at(want)
	if r.placer.total() != 2 {
		t.Errorf("%d placed, want the retry at 09:00", r.placer.total())
	}
}

func TestTheOuterHoursBindAtAdmissionEvenWhenTheCampaignWindowIsWider(t *testing.T) {
	// Outer hours narrowed to 10:00-16:00 under a campaign that was written
	// for 09:00-18:00.
	r := newRig(t, opts{outer: "10:00-16:00", start: ny(t, "2026-06-01 09:30")})
	c := r.newCampaign("Outer", func(s *camSpec) { s.max = 5 })
	r.importContacts(c, numbers("26", 3))
	r.start(c)
	if r.placer.total() != 0 {
		t.Fatalf("%d placed at 09:30, outside the outer hours", r.placer.total())
	}
	r.at(ny(t, "2026-06-01 10:00"))
	if r.placer.total() != 3 {
		t.Errorf("%d placed at 10:00, want 3", r.placer.total())
	}
	r.endAll(keepRinging(numbers("26", 1)[0].number, endAnswered))
	r.at(ny(t, "2026-06-01 16:00"))
	r.importArmed(c, numbers("27", 2))
	if r.placer.total() != 3 {
		t.Errorf("%d placed at 16:00, want none after the outer hours", r.placer.total())
	}
}

func TestAChangedCallingWindowIsAppliedToContactsAlreadyWaiting(t *testing.T) {
	r := newRig(t, opts{outer: "05:00-21:00", start: ny(t, "2026-06-01 07:00")})
	c := r.newCampaign("Widen", func(s *camSpec) { s.max = 5 })
	r.importContacts(c, numbers("28", 3))
	r.start(c)
	if r.placer.total() != 0 {
		t.Fatal("dialled before 09:00")
	}
	svc := campaign.NewService(r.st.Campaigns(), r.dialer(), campaign.Env{OutboundHours: r.cfg.OutboundHours}).
		WithDialer(r.dialer(), r.dialer())
	wider := campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "07:00", End: "18:00"}}}
	if _, err := svc.Update(context.Background(), c.ID, campaign.Patch{CallingWindow: &wider}); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if r.placer.total() != 3 {
		t.Errorf("%d placed after widening the window to start at 07:00, want 3", r.placer.total())
	}

	// And narrowed: the next admission honours it at once.
	r.endAll(keepRinging(numbers("28", 1)[0].number, endAnswered))
	narrow := campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "12:00", End: "18:00"}}}
	if _, err := svc.Update(context.Background(), c.ID, campaign.Patch{CallingWindow: &narrow}); err != nil {
		t.Fatal(err)
	}
	r.importArmed(c, numbers("29", 2))
	if r.placer.total() != 3 {
		t.Errorf("%d placed after narrowing the window to 12:00, want none", r.placer.total())
	}
}

func TestAListOfThousandsOutsideItsWindowIsDeferredOnceNotRefetchedEveryWake(t *testing.T) {
	r := newRig(t, opts{start: ny(t, "2026-06-01 10:00")})
	// Shanghai at 10:00 New York is 22:00: closed until 09:00 next day.
	c := r.newCampaign("Thousands", func(s *camSpec) { s.max = 50; s.zone = "Asia/Shanghai" })
	r.importContacts(c, numbers("30", 3000))
	r.start(c)
	if r.placer.total() != 0 {
		t.Fatal("dialled outside the window")
	}
	open := time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC) // 09:00 Shanghai
	var due, deferred int
	if err := r.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE next_attempt_at <= $2), count(*) FILTER (WHERE next_attempt_at = $3)
		   FROM campaign_contacts WHERE campaign_id = $1`, c.ID, r.clock.Now(), open).Scan(&due, &deferred); err != nil {
		t.Fatal(err)
	}
	if due != 0 || deferred != 3000 {
		t.Fatalf("%d still due and %d deferred to 09:00; want all 3000 deferred so no wake refetches them", due, deferred)
	}
	if p := r.progress(c); p.Activity != campaign.ActivityWaitingWindow || p.NextDialAt == nil || !p.NextDialAt.Equal(open) {
		t.Errorf("progress = %+v", p)
	}
	r.at(open)
	if got := len(r.placer.liveCalls()); got != 50 {
		t.Errorf("%d live when the window opened, want 50", got)
	}
}
