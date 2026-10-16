-- +goose Up
ALTER TYPE refund_status ADD VALUE 'initiated';

ALTER TABLE deposits
    ADD COLUMN notified_at            TIMESTAMPTZ,
    ADD COLUMN notify_attempts        INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN notify_next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX deposits_notify_due_idx ON deposits (notify_next_attempt_at, id)
    WHERE status = 'credited' AND notified_at IS NULL;

ALTER TABLE refunds
    ADD COLUMN initiated_at             TIMESTAMPTZ,
    ADD COLUMN initiate_attempts        INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN initiate_next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT refunds_initiated_check CHECK ((status::text = 'initiated') = (initiated_at IS NOT NULL));

CREATE INDEX refunds_initiate_due_idx ON refunds (initiate_next_attempt_at, id) WHERE initiated_at IS NULL;

-- +goose Down
DROP INDEX refunds_initiate_due_idx;
ALTER TABLE refunds
    DROP CONSTRAINT refunds_initiated_check,
    DROP COLUMN initiate_next_attempt_at,
    DROP COLUMN initiate_attempts,
    DROP COLUMN initiated_at;
DROP INDEX deposits_notify_due_idx;
ALTER TABLE deposits
    DROP COLUMN notify_next_attempt_at,
    DROP COLUMN notify_attempts,
    DROP COLUMN notified_at;
