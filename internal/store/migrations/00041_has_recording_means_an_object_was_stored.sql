-- SPDX-License-Identifier: Apache-2.0
-- cdrs.has_recording means an object was stored, not that recording was
-- intended. The bot ledger used to write the DID's recording setting, and AI
-- outbound calls were never recorded at all, so those rows advertised a
-- recording that cannot be played (#196). Clear the flag wherever no
-- recordings row backs it.

-- +goose Up
UPDATE cdrs
SET has_recording = false
WHERE has_recording
  AND NOT EXISTS (SELECT 1 FROM recordings r WHERE r.call_id = cdrs.call_id);

-- +goose Down
-- A data correction: the rows it cleared held a claim that was never true, and
-- there is no record of which ones they were. Nothing to restore.
SELECT 1;
