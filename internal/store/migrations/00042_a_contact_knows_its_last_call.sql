-- SPDX-License-Identifier: Apache-2.0
-- A contact names its latest finished attempt and that attempt's call
-- (docs/design/10-campaigns.md sections 3.2 and 14), so a lead links to its
-- call and, for a HIGH lead, to the callback that call filed, with no
-- matching on phone digits (#188).
--
-- The finish routine writes both in the transaction that settles the contact,
-- which is the latest attempt's finish. Neither is a foreign key: call_id is
-- not one anywhere (the CDR does not exist while the call is in flight), and
-- an attempt is only ever deleted while DIALING (a void) or with its contact.
-- The backfill therefore reads FINISHED attempts only, the same set the
-- finish routine writes from: a DIALING attempt sets them when it finishes,
-- and one that is voided never existed.
--
-- The callbacks index serves listCallbacks?callId= and the "one callback per
-- call" check of the finish routine, which scanned the table until now.

-- +goose Up
ALTER TABLE campaign_contacts
    ADD COLUMN last_attempt_id uuid,
    ADD COLUMN last_call_id    uuid,
    -- Both or neither: they name one attempt.
    ADD CONSTRAINT ck_campaign_contacts_last_attempt CHECK (
        (last_attempt_id IS NULL) = (last_call_id IS NULL));

UPDATE campaign_contacts c
SET last_attempt_id = a.id, last_call_id = a.call_id
FROM (
    SELECT DISTINCT ON (campaign_contact_id) campaign_contact_id, id, call_id
    FROM campaign_attempts
    WHERE state = 'FINISHED'
    ORDER BY campaign_contact_id, started_at DESC, id DESC
) a
WHERE a.campaign_contact_id = c.id;

CREATE INDEX idx_callbacks_call_id ON callbacks (call_id) WHERE call_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_callbacks_call_id;
ALTER TABLE campaign_contacts
    DROP CONSTRAINT ck_campaign_contacts_last_attempt,
    DROP COLUMN last_call_id,
    DROP COLUMN last_attempt_id;
