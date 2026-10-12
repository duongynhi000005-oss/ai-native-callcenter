-- SPDX-License-Identifier: Apache-2.0
-- The CDR names the campaign a call was placed for (docs/design/10-campaigns.md
-- section 14).
--
-- The value is not computed from anything on the CDR. InsertCDR copies it from
-- the campaign attempt that carries the call's id, in the same statement, so a
-- call that has an attempt has the campaign and every other call has NULL; a
-- fuller revision of the CDR repeats the copy.
--
-- No foreign key, like every other reference a CDR holds (flow_id, queue_id,
-- the agents): the ledger is the record of what happened and must outlive
-- whatever it points at. Only a DRAFT campaign can be deleted, and a draft has
-- no attempts, so nothing is orphaned in practice.
--
-- No backfill: campaigns arrive with this feature, so no existing row can
-- belong to one. The index is partial because almost every row is NULL.

-- +goose Up
ALTER TABLE cdrs ADD COLUMN campaign_id uuid;

CREATE INDEX idx_cdrs_campaign_id_started_at ON cdrs (campaign_id, started_at DESC)
    WHERE campaign_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_cdrs_campaign_id_started_at;
ALTER TABLE cdrs DROP COLUMN campaign_id;
