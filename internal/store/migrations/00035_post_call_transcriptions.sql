-- SPDX-License-Identifier: Apache-2.0

-- Post-call transcription of a recording (issue #27, design 08 section 18).
--
-- One run row per call and a child table of the segments it produced. They are
-- deliberately not rows of `transcripts`: that table is the live transcript,
-- ordered by a dense per-call seq a stream consumer resumes from, and a batch
-- result written into it would collide with the live rows and appear after the
-- call has ended. Here the row is also the work queue (status, attempts,
-- next_attempt_at), so a restart loses nothing.

-- +goose Up
CREATE TABLE call_transcriptions (
    call_id         uuid PRIMARY KEY,
    recording_id    uuid NOT NULL,
    -- Bumped each time a finished run is requested again; the run that wrote
    -- the segments is the one named here.
    run             int NOT NULL DEFAULT 1,
    status          varchar(16) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'RUNNING', 'DONE', 'FAILED')),
    model           text NOT NULL DEFAULT '',
    attempts        int NOT NULL DEFAULT 0,
    error           text NOT NULL DEFAULT '',
    -- The recording's length, which is what the daily cap counts.
    audio_sec       int NOT NULL DEFAULT 0,
    requested_at    timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz
);

CREATE INDEX idx_call_transcriptions_queue
    ON call_transcriptions (next_attempt_at) WHERE status = 'PENDING';
CREATE INDEX idx_call_transcriptions_started_at
    ON call_transcriptions (started_at) WHERE started_at IS NOT NULL;

CREATE TABLE call_transcription_segments (
    call_id    uuid NOT NULL,
    seq        int NOT NULL,
    -- CUSTOMER is the left channel (the caller), FAR_END the right (the bot,
    -- then the agent, following transfers).
    channel    varchar(16) NOT NULL CHECK (channel IN ('CUSTOMER', 'FAR_END')),
    offset_ms  int NOT NULL,
    end_ms     int NOT NULL,
    text       text NOT NULL,
    language   varchar(8) NOT NULL DEFAULT '',
    PRIMARY KEY (call_id, seq)
);

-- +goose Down
DROP TABLE call_transcription_segments;
DROP TABLE call_transcriptions;
