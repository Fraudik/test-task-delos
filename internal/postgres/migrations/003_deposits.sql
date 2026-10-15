-- +goose Up
CREATE TYPE deposit_status AS ENUM ('pending', 'orphaned', 'aml_pending', 'credited', 'refunded');

CREATE TYPE refund_status AS ENUM ('ready_to_send');

CREATE TABLE chain_cursors (
    name       TEXT PRIMARY KEY,
    next_block BIGINT NOT NULL CHECK (next_block >= 0),
    last_hash  TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deposits (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invoice_id          BIGINT NOT NULL REFERENCES invoices (id),
    partner_id          BIGINT NOT NULL REFERENCES partners (id),
    tx_hash             TEXT NOT NULL,
    log_index           INTEGER NOT NULL CHECK (log_index >= 0),
    block_number        BIGINT NOT NULL CHECK (block_number >= 0),
    block_hash          TEXT NOT NULL,
    from_address        TEXT NOT NULL,
    to_address          TEXT NOT NULL,
    amount              BIGINT NOT NULL CHECK (amount > 0),
    fee_bps             INTEGER NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    status              deposit_status NOT NULL DEFAULT 'pending',
    aml_attempts        INTEGER NOT NULL DEFAULT 0,
    aml_next_attempt_at TIMESTAMPTZ,
    aml_risk            TEXT NOT NULL DEFAULT '' CHECK (aml_risk IN ('', 'low', 'high')),
    aml_reason          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tx_hash, log_index),
    CHECK (status <> 'aml_pending' OR aml_next_attempt_at IS NOT NULL)
);

CREATE INDEX deposits_pending_idx ON deposits (block_number, id) WHERE status = 'pending';
CREATE INDEX deposits_aml_due_idx ON deposits (aml_next_attempt_at, id) WHERE status = 'aml_pending';
CREATE INDEX deposits_invoice_idx ON deposits (invoice_id);

CREATE TABLE refunds (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deposit_id BIGINT NOT NULL UNIQUE REFERENCES deposits (id),
    to_address TEXT NOT NULL,
    amount     BIGINT NOT NULL CHECK (amount > 0),
    status     refund_status NOT NULL DEFAULT 'ready_to_send',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE refunds;
DROP TABLE deposits;
DROP TABLE chain_cursors;
DROP TYPE refund_status;
DROP TYPE deposit_status;
