// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// placingSet stands in for outbound.Service.IsPlacing: the placements the
// switch has not confirmed and whose ending is not settled yet.
type placingSet struct {
	mu  sync.Mutex
	ids map[uuid.UUID]outbound.AIDialRequest
}

func (p *placingSet) add(req outbound.AIDialRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[req.CallID] = req
}

func (p *placingSet) settle(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.ids, id)
}

func (p *placingSet) has(id uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.ids[id]
	return ok
}

// A switch that stalls mid-batch (#221). 100 contacts are admitted at once;
// the first 30 originates are confirmed, the other 70 time out. Of those, 20
// turn out to have been placed (their channel appears and the call ends
// normally), 30 never were (the outbound layer writes the placement-failure
// CDR when the window passes) and 20 leave no trace at all (that CDR could not
// be written). No attempt may be counted ORIGINATE_FAILED for a call that was
// placed, no contact may be dialled twice, and each attempt must end with what
// the switch actually did: from its CDR, or as UNKNOWN through the backstop.
func TestAStalledSwitchMidBatchLeavesAttemptsPendingUntilTheSwitchDecides(t *testing.T) {
	r := newRig(t, opts{})
	placing := &placingSet{ids: map[uuid.UUID]outbound.AIDialRequest{}}
	r.stopDialer()
	// As cmd/aicc wires it: a placement still being placed counts as live.
	r.dep.IsLive = func(id uuid.UUID) bool { return r.placer.isLive(id) || placing.has(id) }
	r.startDialer()

	c := r.newCampaign("Stall", func(s *camSpec) { s.max = 100; s.ring = 30 })
	r.importContacts(c, numbers("221", 100))
	var stalled []outbound.AIDialRequest // in the order the switch was asked
	confirmed := 0
	r.placer.setFail(func(req outbound.AIDialRequest) error {
		if confirmed < 30 {
			confirmed++
			return nil
		}
		placing.add(req)
		stalled = append(stalled, req)
		return fmt.Errorf("%w: esl command timed out: bgapi originate …", outbound.ErrPlacementUnconfirmed)
	})
	t0 := r.clock.Now()
	r.start(c)

	r.placer.mu.Lock()
	appeared, neverPlaced, noTrace := stalled[:20], stalled[20:50], stalled[50:]
	r.placer.mu.Unlock()
	if r.placer.total() != 30 || len(stalled) != 70 {
		t.Fatalf("%d confirmed and %d stalled placements, want 30 and 70", r.placer.total(), len(stalled))
	}
	for _, a := range r.attempts(c) {
		if a.State != campaign.AttemptDialing {
			t.Fatalf("attempt %s for %s is %s/%s before the switch said anything", a.CallID, a.Number, a.State, a.Outcome)
		}
	}
	if p := r.progress(c); p.InFlight != 100 {
		t.Errorf("in flight = %d, want all 100: an unconfirmed placement still holds its place", p.InFlight)
	}

	// 5 s in, the stalled switch creates the channels it was asked for first.
	r.clock.Set(t0.Add(5 * time.Second))
	r.placer.mu.Lock()
	for _, req := range appeared {
		pc := placed{Req: req, At: r.clock.Now()}
		r.placer.live[req.CallID] = pc
		r.placer.byNumber[req.To] = req.CallID
	}
	r.placer.mu.Unlock()
	for _, req := range appeared {
		placing.settle(req.CallID)
	}

	// 40 s in, those calls and the confirmed ones end.
	r.clock.Set(t0.Add(40 * time.Second))
	for i, req := range appeared {
		kind := endNoAnswer
		if i%2 == 0 {
			kind = endAnswered
		}
		r.end(placed{Req: req, At: t0.Add(5 * time.Second)}, kind)
	}
	r.endAll(func(string) string { return endBusy })
	r.settle()

	// Past ring timeout + 30 s: the unconfirmed placements are still being
	// placed, so the backstop leaves them alone.
	r.at(t0.Add(61 * time.Second))
	for _, req := range append(append([]outbound.AIDialRequest{}, neverPlaced...), noTrace...) {
		if a, _ := r.st.Attempts().Get(context.Background(), req.CallID); a.State != campaign.AttemptDialing {
			t.Fatalf("attempt for %s settled while its placement was unconfirmed: %+v", req.To, a)
		}
	}

	// 90 s in, the outbound layer's window passes: a placement-failure CDR for
	// the ones it could record, nothing for the rest.
	r.clock.Set(t0.Add(90 * time.Second))
	for _, req := range neverPlaced {
		cdr := cdrFor(req.CallID, req.To, endPlacement, t0, r.clock.Now())
		cdr.HangupCause = outbound.UnconfirmedCause
		if err := r.st.Ledger().InsertCDR(context.Background(), cdr); err != nil {
			t.Fatal(err)
		}
		placing.settle(req.CallID)
	}
	for _, req := range noTrace {
		placing.settle(req.CallID)
	}
	r.settle()

	// The backstop's next look: the attempts with no trace are UNKNOWN.
	r.at(t0.Add(125 * time.Second))

	want := map[uuid.UUID]campaign.Outcome{}
	for i, req := range appeared {
		want[req.CallID] = campaign.OutcomeNoAnswer
		if i%2 == 0 {
			want[req.CallID] = campaign.OutcomeAnswered
		}
	}
	for _, req := range neverPlaced {
		want[req.CallID] = campaign.OutcomeOriginateFailed
	}
	for _, req := range noTrace {
		want[req.CallID] = campaign.OutcomeUnknown
	}
	perNumber := map[string]int{}
	for _, a := range r.attempts(c) {
		perNumber[a.Number]++
		w, isStalled := want[a.CallID]
		if !isStalled {
			w = campaign.OutcomeBusy // a confirmed placement, ended above
		}
		if a.State != campaign.AttemptFinished || a.Outcome != w {
			t.Errorf("%s: %s/%s, want FINISHED/%s", a.Number, a.State, a.Outcome, w)
		}
		if a.Outcome == campaign.OutcomeOriginateFailed && a.HangupCause != outbound.UnconfirmedCause {
			t.Errorf("%s: ORIGINATE_FAILED with cause %q; only a placement that never ran may be", a.Number, a.HangupCause)
		}
	}
	if len(perNumber) != 100 {
		t.Errorf("%d contacts have attempts, want 100", len(perNumber))
	}
	for number, n := range perNumber {
		if n != 1 {
			t.Errorf("%s was dialled %d times", number, n)
		}
	}
	r.noViolations()
}
