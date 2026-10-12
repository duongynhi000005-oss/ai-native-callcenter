// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// maxRefills bounds how many batches one campaign may defer in one pass; a
// list of tens of thousands outside its window is worked through over a few
// passes, not in one long stall of the actor.
const maxRefills = 50

// bucket is the contacts fetched for one campaign in one pass, already judged
// against their calling windows.
type bucket struct {
	ready []campaign.Contact
	// done: nothing more to take this pass (empty, or stopped by a limit).
	done bool
	// blocked: a due, in-window contact waits on a limit.
	blocked string
}

// pass is one look at everything: housekeeping, then round-robin admission,
// then completion and the account of what each campaign is doing.
func (d *Dialer) pass(ctx context.Context) {
	d.isDirty = false
	now := d.clock.Now()
	d.nextWake = now.Add(d.cfg.SafetyScan)

	running, err := d.st.RunningCampaigns(ctx)
	if err != nil {
		d.log.Error("dialer: cannot list running campaigns", "error", err)
		d.nextWake = now.Add(5 * time.Second)
		return
	}
	if d.dncDirty {
		d.dncDirty = false
		d.skipDNC(ctx, now)
	}
	for _, c := range running {
		if d.rearm[c.ID] {
			delete(d.rearm, c.ID)
			if err := d.st.RearmUntried(ctx, c.ID, now); err != nil {
				d.log.Error("dialer: cannot re-arm contacts", "campaignId", c.ID, "error", err)
			}
		}
	}

	live := make([]campaign.Campaign, 0, len(running))
	for _, c := range running {
		if !d.halted[c.ID] {
			live = append(live, c)
		}
	}
	buckets := map[uuid.UUID]*bucket{}
	for _, c := range live {
		buckets[c.ID] = &bucket{}
	}
	if n := len(live); n > 0 {
		// Fair between campaigns: the next call goes to the campaign with the
		// fewest in flight, the rotating start breaking ties. Calls end one by
		// one as well as together, so the share each campaign holds must be
		// restored from what is in flight, not from the order of this pass.
		start := d.rot % n
		d.rot++
		for ctx.Err() == nil {
			var pick *campaign.Campaign
			for i := 0; i < n; i++ {
				c := &live[(start+i)%n]
				if buckets[c.ID].done || d.halted[c.ID] {
					continue
				}
				if pick == nil || d.inflight[c.ID] < d.inflight[pick.ID] {
					pick = c
				}
			}
			if pick == nil {
				break
			}
			d.admitOne(ctx, *pick, buckets[pick.ID], now)
		}
	}

	next := time.Time{}
	seen := map[uuid.UUID]bool{}
	for _, c := range live {
		seen[c.ID] = true
		n := d.account(ctx, c, buckets[c.ID], now)
		if !n.IsZero() && (next.IsZero() || n.Before(next)) {
			next = n
		}
	}
	for id := range d.progress {
		if !seen[id] {
			delete(d.progress, id)
		}
	}
	for _, f := range d.flights {
		if f.nextCheck.Before(next) || next.IsZero() {
			next = f.nextCheck
		}
	}
	if !next.IsZero() && next.Before(d.nextWake) {
		d.nextWake = next
	}
	// A pass that stopped short of the whole list asks for another at once.
	for _, b := range buckets {
		if b.blocked == "" && !b.done {
			d.isDirty = true
		}
	}
}

// admitOne tries to place one call for the campaign and reports whether it
// made progress (placed one, or moved contacts out of the way).
func (d *Dialer) admitOne(ctx context.Context, c campaign.Campaign, b *bucket, now time.Time) bool {
	if len(b.ready) == 0 {
		moved, err := d.refill(ctx, c, b, now)
		if err != nil {
			d.log.Error("dialer: cannot read contacts", "campaignId", c.ID, "error", err)
			b.done = true
			return false
		}
		if len(b.ready) == 0 {
			b.done = true
			return moved
		}
	}
	if reason := d.limit(c); reason != "" {
		b.done, b.blocked = true, reason
		obs.RecordDialerBlocked(reason)
		return false
	}
	contact := b.ready[0]
	b.ready = b.ready[1:]
	d.admit(ctx, c, contact, now)
	return true
}

// limit says which limit forbids one more call for the campaign, if any: the
// conservative rule, which never over-dials (design 10 section 6; expected-
// answer admission is a later slice).
func (d *Dialer) limit(c campaign.Campaign) string {
	if d.inflight[c.ID] >= c.MaxConcurrent {
		return obs.DialerBlockedCampaign
	}
	if d.ringing >= d.cfg.MaxRinging {
		return obs.DialerBlockedRinging
	}
	if d.sl != nil {
		u := d.sl.Usage()
		// Slots in use and reserved are held already; every leg still ringing
		// will want one if it is answered, and so will this one.
		if u.InUse+u.Reserved+d.ringing+1 > u.Cap-u.InboundReserve {
			return obs.DialerBlockedBot
		}
	}
	return ""
}

// refill fetches the next batch of due contacts and judges each against its
// calling window. Contacts held by another campaign's recent touch, or
// outside their window, are moved to the first time they could be dialled and
// never fetched again before then; what remains is ready. It reports whether
// it moved any contact.
func (d *Dialer) refill(ctx context.Context, c campaign.Campaign, b *bucket, now time.Time) (bool, error) {
	free := min(c.MaxConcurrent-d.inflight[c.ID], d.cfg.MaxRinging-d.ringing)
	limit := min(max(free*4, 20), d.cfg.MaxBatch)
	moved := false
	for i := 0; i < maxRefills; i++ {
		var ids []uuid.UUID
		var ats []time.Time

		held, err := d.st.HeldByFrequency(ctx, c.ID, now, d.cfg.Frequency, limit)
		if err != nil {
			return moved, err
		}
		if len(held) > 0 {
			for _, h := range held {
				at := h.LastTouch.Add(d.cfg.Frequency)
				if sched, ok := d.scheduleFor(c, campaign.Contact{TimeZone: h.TimeZone}); ok {
					if open, found := sched.NextOpen(at); found {
						at = open
					}
				}
				ids, ats = append(ids, h.ContactID), append(ats, at)
			}
			if err := d.st.Defer(ctx, ids, ats); err != nil {
				return moved, err
			}
			moved = true
			continue
		}

		due, err := d.st.Due(ctx, c.ID, now, d.cfg.Frequency, limit)
		if err != nil {
			return moved, err
		}
		if len(due) == 0 {
			return moved, nil
		}
		for _, ct := range due {
			sched, ok := d.scheduleFor(c, ct)
			if ok && !sched.IsOpen(now) {
				at, found := sched.NextStart(now)
				if !found {
					at = now.Add(24 * time.Hour)
				}
				ids, ats = append(ids, ct.ID), append(ats, at)
				continue
			}
			b.ready = append(b.ready, ct)
		}
		if len(ids) > 0 {
			if err := d.st.Defer(ctx, ids, ats); err != nil {
				return moved, err
			}
			moved = true
		}
		if len(b.ready) > 0 {
			return moved, nil
		}
	}
	// More is waiting than one pass will sort out.
	d.isDirty = true
	return moved, nil
}

// scheduleFor is the contact's calling schedule: its zone, else the
// campaign's, inside the outer hours. Cached per campaign window and zone.
func (d *Dialer) scheduleFor(c campaign.Campaign, ct campaign.Contact) (campaign.Schedule, bool) {
	key := c.ID.String() + "|" + windowKey(c) + "|" + ct.TimeZone
	if s, ok := d.schedule[key]; ok {
		return s, s.Zone != nil
	}
	if len(d.schedule) > 4096 {
		d.schedule = map[string]campaign.Schedule{}
	}
	s, err := campaign.NewSchedule(c.TimeZone, ct.TimeZone, c.CallingWindow, d.cfg.OutboundHours)
	if err != nil && ct.TimeZone != "" {
		s, err = campaign.NewSchedule(c.TimeZone, "", c.CallingWindow, d.cfg.OutboundHours)
	}
	if err != nil {
		d.log.Warn("dialer: no calling schedule for the contact; its window is not applied",
			"campaignId", c.ID, "zone", ct.TimeZone, "error", err)
		d.schedule[key] = campaign.Schedule{}
		return campaign.Schedule{}, false
	}
	d.schedule[key] = s
	return s, true
}

func windowKey(c campaign.Campaign) string {
	return fmt.Sprintf("%s|%v", c.TimeZone, c.CallingWindow)
}

// admit writes the attempt ahead, then places the call (design 10 section 5.3).
func (d *Dialer) admit(ctx context.Context, c campaign.Campaign, ct campaign.Contact, now time.Time) {
	att, err := d.st.Begin(ctx, ct.ID, now)
	switch {
	case errors.Is(err, campaign.ErrNumberInFlight), errors.Is(err, campaign.ErrContactNotDialable):
		return // someone else has it; it is looked at again when they finish
	case err != nil:
		d.log.Error("dialer: cannot write the attempt ahead", "campaignId", c.ID, "contactId", ct.ID, "error", err)
		return
	}
	ring := time.Duration(c.RingTimeoutSec) * time.Second
	f := &flight{
		attemptID: att.ID, contactID: ct.ID, campaignID: c.ID, callID: att.CallID,
		startedAt: now, ring: ring, nextCheck: now.Add(ring + d.cfg.BackstopMargin),
	}
	d.flights[att.CallID] = f
	d.inflight[c.ID]++
	d.ringing++
	obsInFlight(false, 1)

	pctx, cancel := context.WithTimeout(ctx, d.cfg.DialTimeout)
	defer cancel()
	placed, err := d.pl.PlaceAI(pctx, outbound.AIDialRequest{
		CallID: att.CallID, LegID: att.LegID, To: d.dialString(c, ct.Number), DIDNumber: c.DIDNumber,
		UserData: ct.Slots, RingTimeout: ring, CampaignID: c.ID, AttemptID: att.ID,
	})
	if err == nil {
		if placed.JobID != "" {
			if err := d.st.SetJob(ctx, att.CallID, placed.JobID); err != nil {
				d.log.Warn("dialer: cannot record the originate job", "callId", att.CallID, "error", err)
			}
		}
		return
	}
	d.dialFailed(ctx, c, f, err, now)
}

// dialFailed is what a synchronous error from the placer does to the attempt
// (design 10 section 5.3).
func (d *Dialer) dialFailed(ctx context.Context, c campaign.Campaign, f *flight, err error, now time.Time) {
	switch {
	case errors.Is(err, outbound.ErrAlreadyPlaced):
		return // an earlier drive of this attempt got through
	case errors.Is(err, outbound.ErrNotReady), errors.Is(err, context.Canceled):
		// Nothing was sent: the attempt never happened.
		d.voidAttempt(ctx, f, now)
		return
	}
	verdict := campaign.ClassifyDialError(err, campaign.DialErrorKinds{
		NumberOnDNC: outbound.ErrNumberOnDNC, BadNumber: outbound.ErrBadNumber,
	})
	if verdict.IsVoid {
		d.voidAttempt(ctx, f, now)
		return
	}
	cause := err.Error()
	switch verdict.Outcome {
	case campaign.OutcomeDNCSkipped:
		cause = "NUMBER_ON_DNC"
	case campaign.OutcomeInvalidNumber:
		cause = "INVALID_NUMBER"
	}
	if len(cause) > 200 {
		cause = cause[:200]
	}
	d.log.Warn("dialer: the call could not be placed", "campaignId", c.ID, "callId", f.callID,
		"outcome", verdict.Outcome, "error", err)
	done, ferr := d.st.Finish(ctx, f.callID, campaign.Ending{
		Outcome: verdict.Outcome, HangupCause: cause, EndedAt: now,
	})
	if ferr != nil {
		d.log.Error("dialer: cannot finish the attempt", "callId", f.callID, "error", ferr)
		return
	}
	d.drop(f.callID)
	if done != nil {
		// The breaker is judged now, not when the message comes round: a trunk
		// that is down must stop this pass, not the next one.
		d.judgeBreakers(ctx, *done)
	}
}

func (d *Dialer) voidAttempt(ctx context.Context, f *flight, now time.Time) {
	if err := d.st.Void(ctx, f.callID, now); err != nil {
		d.log.Error("dialer: cannot void the attempt", "callId", f.callID, "error", err)
		return
	}
	d.drop(f.callID)
}

// drop forgets a flight and returns its capacity. It is idempotent.
func (d *Dialer) drop(callID uuid.UUID) *flight {
	f := d.flights[callID]
	if f == nil {
		return nil
	}
	delete(d.flights, callID)
	if d.inflight[f.campaignID]--; d.inflight[f.campaignID] <= 0 {
		delete(d.inflight, f.campaignID)
	}
	if f.isAnswered {
		d.talking--
		obsInFlight(true, -1)
	} else {
		d.ringing--
		obsInFlight(false, -1)
	}
	return f
}

func obsInFlight(isTalking bool, delta int64) {
	kind := obs.DialerRinging
	if isTalking {
		kind = obs.DialerTalking
	}
	obs.DialerInFlightChanged(kind, delta)
}

// onFinished is an attempt's outcome having been written and committed.
func (d *Dialer) onFinished(ctx context.Context, f campaign.Finished) {
	d.drop(f.CallID)
	d.isDirty = true
	if f.IsRevision {
		return
	}
	obs.RecordCampaignAttempt(string(f.Outcome))
	d.judgeBreakers(ctx, f)
}

// judgeBreakers pauses the campaign when its run of failures says the trunk or
// the provider is down (design 10 section 7.4). The run starts at the
// campaign's last start or resume, so a resumed campaign is not paused again
// by the failures that paused it.
func (d *Dialer) judgeBreakers(ctx context.Context, f campaign.Finished) {
	if d.halted[f.CampaignID] {
		return
	}
	since := d.since[f.CampaignID]
	var reason campaign.PauseReason
	switch {
	case campaign.ClassOf(f.Outcome) == campaign.ClassSystem:
		recent, err := d.st.RecentOutcomesSince(ctx, f.CampaignID, since, d.cfg.TrunkBreakerCount)
		if err == nil && campaign.TrunkBreakerTrips(recent, d.cfg.TrunkBreakerCount) {
			reason = campaign.PauseTrunkFailure
		}
	case f.Outcome == campaign.OutcomeBotFailed:
		recent, err := d.st.RecentOutcomesSince(ctx, f.CampaignID, since, max(d.cfg.ProviderBreakerCount*50, 200))
		if err == nil && campaign.ProviderBreakerTrips(recent, d.cfg.ProviderBreakerCount) {
			reason = campaign.PauseProviderFailure
		}
	}
	if reason == "" {
		return
	}
	paused, err := d.st.PauseFor(ctx, f.CampaignID, reason)
	if err != nil {
		d.log.Error("dialer: cannot pause the campaign", "campaignId", f.CampaignID, "error", err)
		return
	}
	if paused {
		d.halted[f.CampaignID] = true
		d.isDirty = true
		d.log.Warn("dialer: campaign paused by a breaker", "campaignId", f.CampaignID, "reason", reason)
	}
}

// account computes what the campaign is doing, completes it when the list is
// exhausted, and returns the earliest time it needs another look.
func (d *Dialer) account(ctx context.Context, c campaign.Campaign, b *bucket, now time.Time) time.Time {
	pending, err := d.st.Pending(ctx, c.ID, now)
	if err != nil {
		d.log.Error("dialer: cannot read the campaign's contacts", "campaignId", c.ID, "error", err)
		return time.Time{}
	}
	if d.halted[c.ID] {
		delete(d.progress, c.ID)
		return time.Time{}
	}
	inflight := d.inflight[c.ID]
	if pending.ActiveTotal == 0 && inflight == 0 && pending.AllTotal > 0 {
		if done, err := d.st.Complete(ctx, c.ID, now); err != nil {
			d.log.Error("dialer: cannot complete the campaign", "campaignId", c.ID, "error", err)
		} else if done {
			d.log.Info("dialer: campaign completed", "campaignId", c.ID)
			delete(d.progress, c.ID)
			return time.Time{}
		}
	}

	p := campaign.Progress{NextDialAt: pending.NextFuture}
	switch {
	case b != nil && b.blocked != "":
		p.Activity = campaign.ActivityAtCapacity
		p.NextDialAt = nil
	case inflight > 0:
		p.Activity = campaign.ActivityDialing
	default:
		p.Activity = campaign.ActivityWaitingRetry
		if first, err := d.st.EarliestPending(ctx, c.ID); err == nil && first != nil {
			if sched, ok := d.scheduleFor(c, *first); ok && !sched.IsOpen(now) {
				p.Activity = campaign.ActivityWaitingWindow
			}
		}
	}
	d.progress[c.ID] = p
	if pending.NextFuture == nil {
		return time.Time{}
	}
	return *pending.NextFuture
}

// publish replaces the read side with the account of each running campaign,
// the in-flight counts as they are now.
func (d *Dialer) publish() {
	view := make(map[uuid.UUID]campaign.Progress, len(d.progress))
	for id, p := range d.progress {
		p.InFlight = d.inflight[id]
		view[id] = p
	}
	d.view.Store(&view)
}

// dialString writes the contact's canonical number in the form the trunk
// expects (AICC_OUTBOUND_DIAL_FORM). The attempt and the contact keep the
// canonical number; only the placement carries the dialled string. When the
// NATIONAL form cannot apply (the number is from another country than the
// campaign's), the number is dialled as E.164 and the campaign is warned
// about once.
func (d *Dialer) dialString(c campaign.Campaign, number string) string {
	cc := c.DefaultCountryCode
	if cc == "" {
		cc = d.cfg.DefaultCountryCode
	}
	out, ok := phonenum.DialString(number, d.cfg.DialForm, cc)
	if !ok && d.cfg.DialForm == phonenum.FormNational && !d.warnedCC[c.ID] {
		d.warnedCC[c.ID] = true
		d.log.Warn("dialer: a number is outside the campaign's country code and is dialled as E.164",
			"campaignId", c.ID, "countryCode", cc)
	}
	return out
}
