// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// An originate is a bgapi command, and a command that fails before the switch
// has made a channel leaves nothing behind: no channel, no hangup, no CDR. The
// only account of it is the BACKGROUND_JOB event carrying the job's result.
// The service keeps the Job-UUID of every originate it issues so that result
// can be matched to the call it belongs to.
//
// One source finishes a call. If the switch created the channel, the CDR
// assembled from its events is the record and the job result is only logged.
// If it never did, this is the only record there will be, and it writes the
// CDR itself.

const (
	// jobMargin is how long past the answer window a job may stay silent
	// before it is given up on: events were lost, an ESL disconnect say.
	// Reconciliation of such a call is a separate piece of work; here it is
	// logged and forgotten.
	jobMargin = 30 * time.Second

	// A result can be dispatched before the bgapi reply that names its job
	// has been handled, because they travel on different goroutines. An
	// unclaimed failure is kept for this long, up to this many, so the
	// originate that follows can still claim it.
	earlyResultTTL = 10 * time.Second
	earlyResultMax = 256
)

// FailureLedger is where a call that never made a channel is recorded. It is
// the same single write path every other CDR goes through.
type FailureLedger interface {
	InsertCDR(ctx context.Context, cdr store.CDR) error
}

// SetFailureLedger says where originate failures are written. Without one the
// failure is logged and the call is still dropped cleanly. Set before the
// service is used.
func (s *Service) SetFailureLedger(l FailureLedger) { s.failures = l }

// trackedJob is one originate awaiting its result.
type trackedJob struct {
	jobID    string
	callID   uuid.UUID
	legID    string
	placedAt time.Time

	// What the CDR of a failed placement says about the call.
	callType string
	from, to string
	did      string
	language string
	flowID   *uuid.UUID
	legKind  string
	legLabel string

	// hadChannel is set when the switch created the leg.
	hadChannel bool
	// isUnconfirmed: the originate timed out, so the Job-UUID is unknown and
	// the placement is tracked by its leg alone (unconfirmed.go).
	isUnconfirmed bool
	timer         *time.Timer
}

type earlyResult struct {
	cause string
	at    time.Time
}

// expectLeg registers a placement before its originate is sent. The leg id is
// minted by us, so it is known long before the bgapi reply names the job; the
// switch's CHANNEL_CREATE for it can be dispatched before that reply is, and
// must find the placement waiting or a call whose channel did exist would be
// taken for one that never did.
func (s *Service) expectLeg(job *trackedJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]*trackedJob{}
		s.legJobs = map[string]*trackedJob{}
	}
	s.legJobs[job.legID] = job
}

// abandonLeg undoes expectLeg when the originate was never sent, and gives the
// callId back.
func (s *Service) abandonLeg(job *trackedJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetJobLocked(job)
	delete(s.placing, job.callID)
}

// attachJob gives an expected leg the Job-UUID bgapi returned. A failure that
// was already dispatched is applied at once.
func (s *Service) attachJob(job *trackedJob, jobID string) {
	s.mu.Lock()
	if jobID == "" {
		// Nothing to correlate with; the CDR alone will tell.
		s.forgetJobLocked(job)
		delete(s.placing, job.callID)
		s.mu.Unlock()
		return
	}
	job.jobID = jobID
	if early, ok := s.early[jobID]; ok {
		delete(s.early, jobID)
		hadChannel := job.hadChannel
		s.forgetJobLocked(job)
		s.mu.Unlock()
		s.log.Info("originate job failed before it was tracked",
			"callId", job.callID, "jobId", jobID, "cause", early.cause)
		s.finishJob(job, hadChannel, early.cause)
		return
	}
	s.jobs[jobID] = job
	job.timer = time.AfterFunc(s.cfg.AnswerWindow+s.margin(), func() { s.jobExpired(job) })
	s.mu.Unlock()
}

// JobCount reports how many originates still await their result.
func (s *Service) JobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// forgetJobLocked removes a job from both indexes.
//
// The claim on its callId (claimCall) is not given back here but where the
// placement's ending is settled: at once when the call exists or nothing was
// sent, and only after the placement-failure CDR is written when it never
// existed, so a retry in between still finds it placed.
func (s *Service) forgetJobLocked(job *trackedJob) {
	if job.jobID != "" && s.jobs[job.jobID] == job {
		delete(s.jobs, job.jobID)
	}
	if s.legJobs[job.legID] == job {
		delete(s.legJobs, job.legID)
	}
	if job.timer != nil {
		job.timer.Stop()
	}
}

// noteChannel records that the switch created a leg of ours.
//
// The leg of an unconfirmed placement is the call arriving after all: its
// answer window starts now, because the switch rings it from now and the
// window armed when the originate timed out may be mostly spent. The leg of a
// placement already given up on is ended at once (unconfirmed.go).
func (s *Service) noteChannel(channelID string) {
	s.mu.Lock()
	if job := s.legJobs[channelID]; job != nil {
		if job.isUnconfirmed && !job.hadChannel {
			if leg := s.pending[channelID]; leg != nil {
				leg.expiresAt = s.now().Add(s.cfg.AnswerWindow)
			}
			s.log.Info("unconfirmed placement made its channel; adopting it",
				"callId", job.callID, "channelId", channelID)
		}
		job.hadChannel = true
	}
	late, isLate := s.takeLateLegLocked(channelID)
	s.mu.Unlock()
	if isLate {
		s.log.Warn("the switch created a leg after its placement was recorded as failed; hanging it up",
			"callId", late.callID, "channelId", channelID, "cause", LateAnswerCause)
		go s.reap(channelID, LateAnswerCause)
	}
}

// handleJob consumes one BACKGROUND_JOB result.
func (s *Service) handleJob(ev telephony.SwitchEvent) {
	s.mu.Lock()
	job := s.jobs[ev.JobID]
	if job == nil && ev.JobChannelID != "" {
		// An unconfirmed placement never learnt its Job-UUID; its result is
		// recognised by the leg the command asked for.
		if j := s.legJobs[ev.JobChannelID]; j != nil && j.isUnconfirmed && j.jobID == "" {
			j.jobID = ev.JobID
			job = j
		}
	}
	if job == nil {
		// Not ours, or not ours yet. Only a failure is worth keeping.
		if !ev.IsJobOK && ev.JobCause != "" {
			s.stashEarlyLocked(ev.JobID, ev.JobCause)
		}
		s.mu.Unlock()
		return
	}
	if !ev.IsJobOK && ev.JobCause == "" {
		// An empty or unreadable body says nothing. The job stays tracked so
		// the backstop still covers it.
		s.mu.Unlock()
		s.log.Warn("originate job result unreadable",
			"callId", job.callID, "jobId", ev.JobID)
		return
	}
	s.forgetJobLocked(job)
	hadChannel := job.hadChannel
	s.mu.Unlock()

	if ev.IsJobOK {
		s.unclaim(job.callID)
		s.log.Debug("originate job ok", "callId", job.callID, "jobId", ev.JobID)
		return
	}
	s.finishJob(job, hadChannel, ev.JobCause)
}

// finishJob acts on a failed job that has already left the map, so it runs
// once.
func (s *Service) finishJob(job *trackedJob, hadChannel bool, cause string) {
	if hadChannel || (s.isLive != nil && s.isLive(job.callID)) {
		// The CDR is the single source for how a call that existed ended.
		s.unclaim(job.callID)
		s.log.Info("originate job failed after the channel existed; the cdr is the record",
			"callId", job.callID, "jobId", job.jobID, "cause", cause)
		return
	}
	s.log.Warn("originate failed before any channel existed",
		"callId", job.callID, "jobId", job.jobID, "cause", cause)
	s.failPlacement(job, cause)
}

func (s *Service) stashEarlyLocked(jobID, cause string) {
	now := time.Now()
	if s.early == nil {
		s.early = map[string]earlyResult{}
	}
	for id, e := range s.early {
		if now.Sub(e.at) > earlyResultTTL {
			delete(s.early, id)
		}
	}
	if len(s.early) >= earlyResultMax {
		return
	}
	s.early[jobID] = earlyResult{cause: cause, at: now}
}

// jobExpired is the backstop: no result and, it turns out, no CDR either.
func (s *Service) jobExpired(job *trackedJob) {
	s.mu.Lock()
	if s.jobs[job.jobID] != job {
		s.mu.Unlock()
		return
	}
	s.forgetJobLocked(job)
	delete(s.placing, job.callID)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if done, err := s.hasCDR(ctx, job.callID); err == nil && done {
		return
	}
	if s.isLive != nil && s.isLive(job.callID) {
		return
	}
	s.lines.Release(job.callID)
	s.log.Warn("originate job never reported and the call has no cdr",
		"callId", job.callID, "jobId", job.jobID, "placedAt", job.placedAt)
}

// failPlacement finishes a call whose originate never made a channel: its
// armed continuation and business data go, and a failed CDR carries the
// cause. The caller has already removed the job from the map, which is what
// makes this run once. The callId is given back once the CDR is written, so
// a retry never finds the call neither placing nor recorded.
func (s *Service) failPlacement(job *trackedJob, cause string) {
	// No channel was ever made, so no call will end to give the line back.
	s.lines.Release(job.callID)
	s.mu.Lock()
	delete(s.pending, job.legID)
	s.mu.Unlock()

	var userData map[string]any
	if s.callData != nil {
		userData = s.callData.CallData(job.callID)
		s.callData.Drop(job.callID)
	}
	if s.failures == nil {
		s.unclaim(job.callID)
		return
	}

	cdr := placementFailureCDR(job, cause, userData, time.Now().UTC())

	// Off the dispatch loop: it must not wait on a database.
	go func() {
		defer s.unclaim(job.callID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// A ledger row that already exists saw more of the call than this
		// does; never write over it.
		if done, err := s.hasCDR(ctx, job.callID); err == nil && done {
			return
		}
		if err := s.failures.InsertCDR(ctx, cdr); err != nil {
			s.log.Error("could not write the cdr of a failed originate",
				"callId", job.callID, "error", err)
		}
	}()
}

// placementFailureCDR is the ledger row for a call whose originate never made
// a channel. Tech.isPlacementFailure marks it as the row that knows least: the
// store never lets it replace another row, and any other row replaces it.
func placementFailureCDR(job *trackedJob, cause string, userData map[string]any, now time.Time) store.CDR {
	cdr := store.CDR{
		CallID:      job.callID,
		StartedAt:   job.placedAt,
		EndedAt:     now,
		CallType:    job.callType,
		Language:    job.language,
		FromNumber:  job.from,
		ToNumber:    job.to,
		DID:         job.did,
		FlowID:      job.flowID,
		Status:      store.CDRStatusFailed,
		HangupCause: cause,
		UserData:    userData,
		Tech: map[string]any{
			"isPlacementFailure":  true,
			"originateJobId":      job.jobID,
			"originatedChannelId": job.legID,
		},
		Legs: []store.Leg{{Kind: job.legKind, Label: job.legLabel, Note: cause}},
	}
	cdr.TotalSec = max(0, int(now.Sub(job.placedAt).Seconds()))
	return cdr
}
