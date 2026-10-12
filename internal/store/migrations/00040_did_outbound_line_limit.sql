-- SPDX-License-Identifier: Apache-2.0
-- How many outbound calls a number may carry at once (docs/design/10-campaigns.md
-- section 6.6). A carrier sells lines per number; the figure is a fact of the
-- number's contract, so it lives with the number and is maintained through /dids.
--
-- NULL means no limit, which is what every existing row keeps. Inbound lines
-- are a separate matter and are neither counted nor limited here.

-- +goose Up
ALTER TABLE dids
    ADD COLUMN max_outbound_channels integer,
    ADD CONSTRAINT ck_dids_max_outbound_channels CHECK (max_outbound_channels IS NULL OR max_outbound_channels > 0);

-- +goose Down
ALTER TABLE dids
    DROP CONSTRAINT ck_dids_max_outbound_channels,
    DROP COLUMN max_outbound_channels;
