// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// Post-call transcription run states. The strings are the ones the API serves
// (PostCallTranscriptionStatus).
const (
	TranscriptionPending = "PENDING"
	TranscriptionRunning = "RUNNING"
	TranscriptionDone    = "DONE"
	TranscriptionFailed  = "FAILED"
)

// The two recorded channels. CUSTOMER is the left channel; FAR_END is the
// right one, which is the bot and then the agent as the call moves on.
const (
	ChannelCustomer = "CUSTOMER"
	ChannelFarEnd   = "FAR_END"
)

// CallTranscription is the post-call transcription of one call: the run, and
// the work-queue row it doubles as.
type CallTranscription struct {
	CallID        uuid.UUID
	RecordingID   uuid.UUID
	Run           int
	Status        string
	Model         string
	Attempts      int
	Error         string
	AudioSec      int
	RequestedAt   time.Time
	NextAttemptAt time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// TranscriptionSegment is one recognised stretch of speech on one channel.
type TranscriptionSegment struct {
	Seq      int
	Channel  string
	OffsetMs int
	EndMs    int
	Text     string
	Language string
}

// AwaitingTranscription is a recorded call that has no run yet.
type AwaitingTranscription struct {
	CallID      uuid.UUID
	RecordingID uuid.UUID
	DurationSec int
}

func transcriptionFromRow(r queries.CallTranscription) CallTranscription {
	out := CallTranscription{
		CallID: r.CallID, RecordingID: r.RecordingID, Run: int(r.Run),
		Status: r.Status, Model: r.Model, Attempts: int(r.Attempts), Error: r.Error,
		AudioSec: int(r.AudioSec), RequestedAt: r.RequestedAt.Time, NextAttemptAt: r.NextAttemptAt.Time,
	}
	if r.StartedAt.Valid {
		t := r.StartedAt.Time
		out.StartedAt = &t
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		out.FinishedAt = &t
	}
	return out
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// EnqueueTranscription queues a call's recording for post-call transcription.
// It is idempotent: it reports false, and changes nothing, when the call
// already has a run.
func (l *LedgerStore) EnqueueTranscription(ctx context.Context, callID, recordingID uuid.UUID, audioSec int) (bool, error) {
	_, err := l.q.EnqueueCallTranscription(ctx, queries.EnqueueCallTranscriptionParams{
		CallID: callID, RecordingID: recordingID, AudioSec: int32(audioSec),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RequeueTranscription starts a fresh run over a finished one (DONE or
// FAILED). It reports false when the call's run is pending or running, which a
// second request must not disturb, or when there is no run at all.
func (l *LedgerStore) RequeueTranscription(ctx context.Context, callID, recordingID uuid.UUID, audioSec int) (bool, error) {
	_, err := l.q.RequeueCallTranscription(ctx, queries.RequeueCallTranscriptionParams{
		CallID: callID, RecordingID: recordingID, AudioSec: int32(audioSec),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// GetTranscription reads a call's run; pgx.ErrNoRows when it has none.
func (l *LedgerStore) GetTranscription(ctx context.Context, callID uuid.UUID) (CallTranscription, error) {
	row, err := l.q.GetCallTranscription(ctx, callID)
	if err != nil {
		return CallTranscription{}, err
	}
	return transcriptionFromRow(row), nil
}

// TranscriptionSegments lists a call's segments in order.
func (l *LedgerStore) TranscriptionSegments(ctx context.Context, callID uuid.UUID) ([]TranscriptionSegment, error) {
	rows, err := l.q.ListCallTranscriptionSegments(ctx, callID)
	if err != nil {
		return nil, err
	}
	out := make([]TranscriptionSegment, 0, len(rows))
	for _, r := range rows {
		out = append(out, TranscriptionSegment{
			Seq: int(r.Seq), Channel: r.Channel, OffsetMs: int(r.OffsetMs),
			EndMs: int(r.EndMs), Text: r.Text, Language: r.Language,
		})
	}
	return out, nil
}

// ClaimTranscription takes the oldest due run for a worker, marking it RUNNING
// and counting the attempt. ok is false when nothing is due.
func (l *LedgerStore) ClaimTranscription(ctx context.Context, model string) (CallTranscription, bool, error) {
	row, err := l.q.ClaimCallTranscription(ctx, model)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallTranscription{}, false, nil
	}
	if err != nil {
		return CallTranscription{}, false, err
	}
	return transcriptionFromRow(row), true, nil
}

// ResetRunningTranscriptions returns runs a dead process left RUNNING to the
// queue.
func (l *LedgerStore) ResetRunningTranscriptions(ctx context.Context) (int64, error) {
	return l.q.ResetRunningCallTranscriptions(ctx)
}

// CompleteTranscription replaces the call's segments and marks the run DONE,
// together. It reports false, writing nothing, when the run is no longer the
// one RUNNING (it was superseded).
func (l *LedgerStore) CompleteTranscription(ctx context.Context, callID uuid.UUID, run int, segs []TranscriptionSegment) (bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := l.q.WithTx(tx)

	n, err := q.CompleteCallTranscription(ctx, queries.CompleteCallTranscriptionParams{CallID: callID, Run: int32(run)})
	if err != nil || n == 0 {
		return false, err
	}
	if err := q.DeleteCallTranscriptionSegments(ctx, callID); err != nil {
		return false, err
	}
	params := make([]queries.InsertCallTranscriptionSegmentsParams, 0, len(segs))
	for _, s := range segs {
		params = append(params, queries.InsertCallTranscriptionSegmentsParams{
			CallID: callID, Seq: int32(s.Seq), Channel: s.Channel,
			OffsetMs: int32(s.OffsetMs), EndMs: int32(s.EndMs), Text: s.Text, Language: s.Language,
		})
	}
	if _, err := q.InsertCallTranscriptionSegments(ctx, params); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// RetryTranscription puts a failed attempt back in the queue, due at retryAt.
func (l *LedgerStore) RetryTranscription(ctx context.Context, callID uuid.UUID, run int, reason string, retryAt time.Time) error {
	_, err := l.q.RetryCallTranscription(ctx, queries.RetryCallTranscriptionParams{
		CallID: callID, Run: int32(run), Error: reason, NextAttemptAt: ts(retryAt),
	})
	return err
}

// FailTranscription ends a run for good.
func (l *LedgerStore) FailTranscription(ctx context.Context, callID uuid.UUID, run int, reason string) error {
	_, err := l.q.FailCallTranscription(ctx, queries.FailCallTranscriptionParams{
		CallID: callID, Run: int32(run), Error: reason,
	})
	return err
}

// TranscribedAudioSecSince is the audio put to the vendor since a moment.
func (l *LedgerStore) TranscribedAudioSecSince(ctx context.Context, since time.Time) (int, error) {
	n, err := l.q.AudioSecStartedSince(ctx, ts(since))
	return int(n), err
}

// LiveTranscriptSpeakers lists the speakers that have a line in the call's
// live transcript, whichever source wrote it.
func (l *LedgerStore) LiveTranscriptSpeakers(ctx context.Context, callID uuid.UUID) ([]string, error) {
	return l.q.LiveTranscriptSpeakers(ctx, callID)
}

// CallsAwaitingTranscription lists recorded calls since a moment that have no
// post-call run, newest first.
func (l *LedgerStore) CallsAwaitingTranscription(ctx context.Context, since time.Time, limit int) ([]AwaitingTranscription, error) {
	rows, err := l.q.CallsAwaitingTranscription(ctx, queries.CallsAwaitingTranscriptionParams{
		CreatedAt: ts(since), Limit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]AwaitingTranscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, AwaitingTranscription{CallID: r.CallID, RecordingID: r.RecordingID, DurationSec: int(r.DurationSec)})
	}
	return out, nil
}
