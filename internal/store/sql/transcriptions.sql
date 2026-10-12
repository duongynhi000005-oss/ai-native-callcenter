-- SPDX-License-Identifier: Apache-2.0

-- Post-call transcription: one run row per call (also the work queue) and the
-- segments it produced. See migration 00035 and design 08 section 18.

-- name: EnqueueCallTranscription :one
-- Idempotent: a call that already has a run keeps it. Returns the row only
-- when this call inserted it.
INSERT INTO call_transcriptions (call_id, recording_id, audio_sec)
VALUES ($1, $2, $3)
ON CONFLICT (call_id) DO NOTHING
RETURNING *;

-- name: RequeueCallTranscription :one
-- A fresh run over a finished one. Only DONE and FAILED rows can be requeued,
-- so a request that races a run in flight changes nothing.
UPDATE call_transcriptions
SET status = 'PENDING', run = run + 1, attempts = 0, error = '',
    recording_id = $2, audio_sec = $3,
    requested_at = now(), next_attempt_at = now(),
    started_at = NULL, finished_at = NULL
WHERE call_id = $1 AND status IN ('DONE', 'FAILED')
RETURNING *;

-- name: GetCallTranscription :one
SELECT * FROM call_transcriptions WHERE call_id = $1;

-- name: ListCallTranscriptionSegments :many
SELECT * FROM call_transcription_segments WHERE call_id = $1 ORDER BY seq;

-- name: ClaimCallTranscription :one
-- The oldest due row, taken without blocking other workers.
UPDATE call_transcriptions
SET status = 'RUNNING', attempts = attempts + 1, started_at = now(),
    finished_at = NULL, model = $1
WHERE call_id = (
    SELECT t.call_id FROM call_transcriptions t
    WHERE t.status = 'PENDING' AND t.next_attempt_at <= now()
    ORDER BY t.next_attempt_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1)
RETURNING *;

-- name: ResetRunningCallTranscriptions :execrows
-- Rows a crashed process left RUNNING go back to the queue. Their attempt
-- stays counted, so a recording that kills the process cannot loop forever.
UPDATE call_transcriptions
SET status = 'PENDING', next_attempt_at = now()
WHERE status = 'RUNNING';

-- name: CompleteCallTranscription :execrows
-- Guarded by run, so a run that was superseded cannot overwrite its successor.
UPDATE call_transcriptions
SET status = 'DONE', error = '', finished_at = now()
WHERE call_id = $1 AND run = $2 AND status = 'RUNNING';

-- name: RetryCallTranscription :execrows
UPDATE call_transcriptions
SET status = 'PENDING', error = $3, next_attempt_at = $4
WHERE call_id = $1 AND run = $2 AND status = 'RUNNING';

-- name: FailCallTranscription :execrows
UPDATE call_transcriptions
SET status = 'FAILED', error = $3, finished_at = now()
WHERE call_id = $1 AND run = $2 AND status = 'RUNNING';

-- name: DeleteCallTranscriptionSegments :exec
DELETE FROM call_transcription_segments WHERE call_id = $1;

-- name: InsertCallTranscriptionSegments :copyfrom
INSERT INTO call_transcription_segments (call_id, seq, channel, offset_ms, end_ms, text, language)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: AudioSecStartedSince :one
-- Audio put to the vendor since a moment: what the cap counts. A run that
-- failed counts too, because the vendor may have billed it.
SELECT coalesce(sum(audio_sec), 0)::bigint FROM call_transcriptions
WHERE started_at >= $1;

-- name: LiveTranscriptSpeakers :many
-- Who has a line in the live transcript of a call, of either source.
SELECT DISTINCT speaker FROM transcripts WHERE call_id = $1 AND kind = 'TEXT';

-- name: CallsAwaitingTranscription :many
-- Backfill: recorded calls with no run, newest first.
SELECT r.call_id, r.id AS recording_id, r.duration_sec
FROM recordings r
WHERE r.deleted_at IS NULL
  AND r.created_at >= $1
  AND NOT EXISTS (SELECT 1 FROM call_transcriptions t WHERE t.call_id = r.call_id)
ORDER BY r.created_at DESC
LIMIT $2;
