// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// An unconfirmed placement (#221).
//
// An originate is written to the switch as a bgapi command and the reply only
// says the job was accepted. When that reply does not arrive within the ESL
// command timeout, nothing is known: the switch may be stalled with the
// command queued on its socket, and it runs the originate when it recovers,
// a minute or two later in the incident that found this. Treating the timeout
// as "not taken" told the client the call failed while the customer's phone
// rang, left the late leg parked with nobody to bridge it, and let a retry
// with the same callId dial a second time.
//
// So a timed-out originate is neither a success nor a failure. Everything a
// placed call holds stays held under its already-minted leg id: the line, the
// business data, the continuation armed for the answer, and the placement
// itself, which the idempotency check consults. The caller is told
// ErrPlacementUnconfirmed (the API answers 202). Then the switch decides:
//
//   - the leg's channel appears within the window: it is the call. Its answer
//     window is counted from the channel's creation, since the switch rings it
//     from then, and its CDR comes from its events like any call's;
//   - the job's result arrives (matched by the leg id the command carried,
//     because its Job-UUID was in the reply that never came): a failure before
//     any channel is a placement failure with the switch's own cause;
//   - nothing within the window (the answer window plus the job margin): the
//     placement failed, and it gets the placement-failure CDR with
//     UnconfirmedCause;
//   - a channel for it that turns up after that is ended at once (see
//     lateLegs): the platform has already recorded the call as not placed, a
//     campaign may already have scheduled the contact again, and a leg nobody
//     will bridge must not be left parked on a person.

// ErrPlacementUnconfirmed means the originate was written to the switch and the
// switch did not confirm it in time. The call may still be placed; it is
// tracked under its callId, a retry with that callId answers ErrAlreadyPlaced,
// and if the switch never places it a FAILED CDR with UnconfirmedCause is
// written once the window has passed. The CallID is set alongside it.
var ErrPlacementUnconfirmed = errors.New("outbound: the switch has not confirmed the placement")

// UnconfirmedCause is the hangupCause of the placement-failure CDR of an
// unconfirmed placement whose channel never appeared within its window. It is
// this platform's own word, not the switch's.
const UnconfirmedCause = "PLACEMENT_UNCONFIRMED"

const (
	// lateLegFor is how long the leg of a placement given up on is remembered,
	// so that a switch catching up even later is still recognised.
	lateLegFor = 10 * time.Minute
	// lateLegMax bounds that memory; the oldest entries go first.
	lateLegMax = 4096
)

// isUnconfirmed reports an originate error that leaves the outcome unknown.
func isUnconfirmed(err error) bool { return errors.Is(err, esl.ErrTimeout) }

// unconfirmedError is what Dial and PlaceAI return for an unconfirmed
// placement: ErrPlacementUnconfirmed, with the switch's error kept for logs.
func unconfirmedError(err error) error {
	return fmt.Errorf("%w: %w", ErrPlacementUnconfirmed, err)
}

// checkPlaced is the idempotency check of a placement. A callId this process
// is placing (whether the switch has confirmed it or not), one the ledger
// already holds and one that is live are all ErrAlreadyPlaced: a finished call
// stays finished, a running call keeps running, and neither redials. A zero
// requested id mints a fresh one, which nothing can hold yet.
func (s *Service) checkPlaced(ctx context.Context, requested uuid.UUID) (uuid.UUID, error) {
	if requested == uuid.Nil {
		return uuid.Must(uuid.NewV7()), nil
	}
	if s.IsPlacing(requested) {
		return requested, ErrAlreadyPlaced
	}
	done, err := s.hasCDR(ctx, requested)
	if err != nil {
		return uuid.Nil, err
	}
	if done || s.isLiveCall(requested) {
		return requested, ErrAlreadyPlaced
	}
	return requested, nil
}

// claimCall makes the check hold under concurrency: the callId is claimed for
// one placement, until its ending is settled (the job's result, the window,
// the placement-failure CDR, or unclaim when nothing was sent). Two requests
// that race with one callId dial once; the second is ErrAlreadyPlaced. It is
// taken after the pre-send refusals (do-not-call), so those answer every
// request alike, and before anything is held under the callId (the line, the
// business data).
func (s *Service) claimCall(callID uuid.UUID) error {
	s.mu.Lock()
	if _, busy := s.placing[callID]; busy {
		s.mu.Unlock()
		return ErrAlreadyPlaced
	}
	if s.placing == nil {
		s.placing = map[uuid.UUID]struct{}{}
	}
	s.placing[callID] = struct{}{}
	s.mu.Unlock()
	// Asked again with the claim held (and not under the lock: it asks the
	// registry): a placement settled since checkPlaced is by now a live call,
	// or one whose placement-failure CDR is written, since it stayed claimed
	// until it was.
	if s.isLiveCall(callID) {
		s.unclaim(callID)
		return ErrAlreadyPlaced
	}
	return nil
}

// unclaim gives a callId back: its placement sent nothing, or its ending is
// settled.
func (s *Service) unclaim(callID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.placing, callID)
}

// IsPlacing reports whether this process is still placing the call: the callId
// is claimed and the placement's ending not yet settled, whether the switch
// confirmed the originate or not. The
// dialer treats such a call as live, so its backstop waits for what the switch
// does instead of settling an attempt the customer may be answering.
func (s *Service) IsPlacing(callID uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.placing[callID]
	return ok
}

// holdUnconfirmed keeps an expected leg tracked after its originate timed out,
// for the answer window plus the job margin, with onAnswer armed for as long:
// its channel may appear at any point of that window, and its own answer
// window is counted from then (noteChannel). A placement that a job result
// already finished is left alone.
func (s *Service) holdUnconfirmed(job *trackedJob, onAnswer func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legJobs[job.legID] != job {
		return
	}
	window := s.cfg.AnswerWindow + s.margin()
	s.pending[job.legID] = &pendingLeg{onAnswer: onAnswer, expiresAt: s.now().Add(window)}
	job.isUnconfirmed = true
	job.timer = time.AfterFunc(window, func() { s.unconfirmedExpired(job) })
}

// unconfirmedExpired ends the wait for an unconfirmed placement. If its channel
// appeared the call's own record covers it; otherwise it never was a call.
func (s *Service) unconfirmedExpired(job *trackedJob) {
	s.mu.Lock()
	if s.legJobs[job.legID] != job {
		s.mu.Unlock()
		return
	}
	hadChannel := job.hadChannel
	s.forgetJobLocked(job)
	if !hadChannel {
		// Before the lock is let go, so a CHANNEL_CREATE dispatched from now
		// on finds the leg either tracked or given up, never neither.
		s.rememberLateLegLocked(job.legID, job.callID)
	}
	s.mu.Unlock()

	if hadChannel || s.isLiveCall(job.callID) {
		s.unclaim(job.callID)
		s.log.Info("unconfirmed placement made its channel; the cdr is the record",
			"callId", job.callID, "channelId", job.legID)
		return
	}
	s.log.Warn("unconfirmed placement made no channel within its window; recording it as failed",
		"callId", job.callID, "channelId", job.legID, "placedAt", job.placedAt, "cause", UnconfirmedCause)
	s.failPlacement(job, UnconfirmedCause)
}

// lateLeg is a leg given up on: its placement is recorded as failed.
type lateLeg struct {
	callID uuid.UUID
	at     time.Time
}

func (s *Service) rememberLateLegLocked(legID string, callID uuid.UUID) {
	now := s.now()
	if s.lateLegs == nil {
		s.lateLegs = map[string]lateLeg{}
	}
	for id, l := range s.lateLegs {
		if now.Sub(l.at) >= lateLegFor {
			delete(s.lateLegs, id)
		}
	}
	for len(s.lateLegs) >= lateLegMax {
		var oldest string
		var at time.Time
		for id, l := range s.lateLegs {
			if at.IsZero() || l.at.Before(at) {
				oldest, at = id, l.at
			}
		}
		delete(s.lateLegs, oldest)
	}
	s.lateLegs[legID] = lateLeg{callID: callID, at: now}
}

// takeLateLegLocked reports, once, a channel that is the leg of a placement
// given up on.
func (s *Service) takeLateLegLocked(channelID string) (lateLeg, bool) {
	l, ok := s.lateLegs[channelID]
	if !ok {
		return lateLeg{}, false
	}
	delete(s.lateLegs, channelID)
	if s.now().Sub(l.at) >= lateLegFor {
		return lateLeg{}, false
	}
	return l, true
}

func (s *Service) margin() time.Duration {
	if s.jobMargin > 0 {
		return s.jobMargin
	}
	return jobMargin
}
