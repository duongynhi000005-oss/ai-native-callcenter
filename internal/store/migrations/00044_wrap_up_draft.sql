-- SPDX-License-Identifier: Apache-2.0
-- A wrap-up can carry an AI draft, kept apart from what the agent files.
--
-- The draft is the suggestion; disposition_code / note stay what the agent
-- confirmed (or the defaults the platform opened the record with). Keeping them
-- in separate columns, and keeping the draft after confirmation, is what lets a
-- report say what was suggested against what was filed.
--
-- draft_generated_at is the draft's existence: NULL means no draft was ever
-- written for this record, and a draft with no suggested disposition is still a
-- draft (draft_disposition_code NULL, draft_note set). Every existing row has
-- none, so nothing is backfilled.

-- +goose Up
ALTER TABLE wrap_ups ADD COLUMN draft_disposition_code text;
ALTER TABLE wrap_ups ADD COLUMN draft_note text;
ALTER TABLE wrap_ups ADD COLUMN draft_generated_at timestamptz;

-- +goose Down
ALTER TABLE wrap_ups DROP COLUMN draft_generated_at;
ALTER TABLE wrap_ups DROP COLUMN draft_note;
ALTER TABLE wrap_ups DROP COLUMN draft_disposition_code;
