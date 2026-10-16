-- +goose Up
CREATE TYPE invoice_status AS ENUM ('open', 'paid');

ALTER TABLE invoices
    ADD COLUMN amount                 BIGINT,
    ADD COLUMN fee_bps                INTEGER,
    ADD COLUMN fee_fixed              BIGINT,
    ADD COLUMN status                 invoice_status NOT NULL DEFAULT 'open',
    ADD COLUMN paid_at                TIMESTAMPTZ,
    ADD COLUMN notified_at            TIMESTAMPTZ,
    ADD COLUMN notify_attempts        INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN notify_next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

UPDATE invoices i
SET amount = 1, fee_bps = p.fee_bps, fee_fixed = p.fee_fixed
FROM partners p
WHERE p.id = i.partner_id;

UPDATE invoices i
SET status = 'paid', paid_at = now(), notified_at = now()
WHERE EXISTS (SELECT 1 FROM deposits d WHERE d.invoice_id = i.id AND d.status = 'credited');

ALTER TABLE invoices
    ALTER COLUMN amount SET NOT NULL,
    ALTER COLUMN fee_bps SET NOT NULL,
    ALTER COLUMN fee_fixed SET NOT NULL,
    ADD CONSTRAINT invoices_amount_check CHECK (amount > 0),
    ADD CONSTRAINT invoices_fee_bps_check CHECK (fee_bps BETWEEN 0 AND 10000),
    ADD CONSTRAINT invoices_fee_fixed_check CHECK (fee_fixed >= 0),
    ADD CONSTRAINT invoices_paid_check CHECK ((status = 'paid') = (paid_at IS NOT NULL));

CREATE INDEX invoices_notify_due_idx ON invoices (notify_next_attempt_at, id)
    WHERE status = 'paid' AND notified_at IS NULL;

DROP INDEX deposits_notify_due_idx;
DROP INDEX deposits_confirmed_idx;
CREATE INDEX deposits_confirmed_invoice_idx ON deposits (invoice_id) WHERE status = 'confirmed';

ALTER TABLE deposits
    DROP COLUMN fee_bps,
    DROP COLUMN fee_fixed,
    DROP COLUMN notified_at,
    DROP COLUMN notify_attempts,
    DROP COLUMN notify_next_attempt_at;

-- +goose Down
ALTER TABLE deposits
    ADD COLUMN fee_bps                INTEGER NOT NULL DEFAULT 0 CHECK (fee_bps BETWEEN 0 AND 10000),
    ADD COLUMN fee_fixed              BIGINT NOT NULL DEFAULT 0 CHECK (fee_fixed >= 0),
    ADD COLUMN notified_at            TIMESTAMPTZ,
    ADD COLUMN notify_attempts        INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN notify_next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

DROP INDEX deposits_confirmed_invoice_idx;
CREATE INDEX deposits_confirmed_idx ON deposits (id) WHERE status = 'confirmed';
CREATE INDEX deposits_notify_due_idx ON deposits (notify_next_attempt_at, id)
    WHERE status = 'credited' AND notified_at IS NULL;
DROP INDEX invoices_notify_due_idx;

ALTER TABLE invoices
    DROP CONSTRAINT invoices_paid_check,
    DROP COLUMN notify_next_attempt_at,
    DROP COLUMN notify_attempts,
    DROP COLUMN notified_at,
    DROP COLUMN paid_at,
    DROP COLUMN status,
    DROP COLUMN fee_fixed,
    DROP COLUMN fee_bps,
    DROP COLUMN amount;

DROP TYPE invoice_status;
