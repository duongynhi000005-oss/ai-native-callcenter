-- SPDX-License-Identifier: Apache-2.0
-- A callback the bot filed with words for its number ("当前来电号码", the
-- model's description of the caller's own number) gets the caller's number
-- back (#311). take_message now discards such a value and calls back the
-- caller's number instead; this repairs the rows written before it did.
--
-- Only a phone_number with no digit at all is touched: it can never be
-- dialled, and the caller's number is what the bot would write today. A value
-- that holds digits but is not a valid number (a digit short, an extension
-- after it) is left as it is, because which digits were meant is a person's
-- call, not a migration's. So is a callback whose call has no ledger row, or
-- whose row has no number with a digit in it.
--
-- The caller's number is the far end the bot spoke to, which is what it
-- falls back to at runtime (the ANI): on a call this platform placed to a
-- customer the CDR reads from the DID to the customer, so it is to_number;
-- on every other call (one that came in, or an agent's call to a bot DID,
-- where from_number is the DID's caller and to_number is the DID) it is
-- from_number.

-- +goose Up
UPDATE callbacks AS cb
SET phone_number = n.caller
FROM (
    SELECT call_id,
           CASE
               WHEN call_type = 'OUTBOUND' AND did <> '' AND from_number = did
                   THEN to_number
               ELSE from_number
           END AS caller
    FROM cdrs
) AS n
WHERE cb.call_id = n.call_id
  AND cb.phone_number !~ '[0-9]'
  AND n.caller ~ '[0-9]';

-- +goose Down
-- Nothing to undo: the words that were replaced are not kept, and putting a
-- value nobody can dial back into a work queue would serve nobody. The rows
-- keep the caller's number, which the code at the previous version also
-- writes when the model gives no number.
SELECT 1;
