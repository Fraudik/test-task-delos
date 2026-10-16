-- +goose Up
CREATE TYPE deposit_status AS ENUM ('pending', 'orphaned', 'confirmed', 'credited', 'refunded');

CREATE TYPE refund_status AS ENUM ('ready_to_send');

CREATE TABLE chain_cursors (
    name       TEXT PRIMARY KEY,
    next_block BIGINT NOT NULL CHECK (next_block >= 0),
    last_hash  TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deposits (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invoice_id   BIGINT NOT NULL REFERENCES invoices (id),
    partner_id   BIGINT NOT NULL REFERENCES partners (id),
    tx_hash      TEXT NOT NULL,
    log_index    INTEGER NOT NULL CHECK (log_index >= 0),
    block_number BIGINT NOT NULL CHECK (block_number >= 0),
    block_hash   TEXT NOT NULL,
    from_address TEXT NOT NULL,
    to_address   TEXT NOT NULL,
    amount       BIGINT NOT NULL CHECK (amount > 0),
    fee_bps      INTEGER NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    status       deposit_status NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tx_hash, log_index)
);

CREATE INDEX deposits_pending_idx ON deposits (block_number, id) WHERE status = 'pending';
CREATE INDEX deposits_confirmed_idx ON deposits (id) WHERE status = 'confirmed';
CREATE INDEX deposits_invoice_idx ON deposits (invoice_id);

CREATE TABLE deposit_aml_checks (
    deposit_id      BIGINT PRIMARY KEY REFERENCES deposits (id),
    risk            TEXT NOT NULL DEFAULT '' CHECK (risk IN ('', 'low', 'high')),
    reason          TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ DEFAULT now(),
    checked_at      TIMESTAMPTZ,
    CHECK ((risk = '') = (next_attempt_at IS NOT NULL)),
    CHECK ((risk = '') = (checked_at IS NULL))
);

CREATE INDEX deposit_aml_checks_due_idx ON deposit_aml_checks (next_attempt_at, deposit_id) WHERE risk = '';

-- +goose StatementBegin
CREATE FUNCTION deposits_check_aml_risk() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    aml_risk TEXT;
BEGIN
    IF NEW.status IN ('credited', 'refunded') THEN
        SELECT risk INTO aml_risk FROM deposit_aml_checks WHERE deposit_id = NEW.id;
        IF (NEW.status = 'credited' AND aml_risk IS DISTINCT FROM 'low')
            OR (NEW.status = 'refunded' AND aml_risk IS DISTINCT FROM 'high') THEN
            RAISE EXCEPTION 'deposit % cannot be % with AML risk %', NEW.id, NEW.status, coalesce(nullif(aml_risk, ''), 'unknown')
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER deposits_aml_consistency
    BEFORE UPDATE OF status ON deposits
    FOR EACH ROW EXECUTE FUNCTION deposits_check_aml_risk();

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
DROP TRIGGER deposits_aml_consistency ON deposits;
DROP FUNCTION deposits_check_aml_risk();
DROP TABLE deposit_aml_checks;
DROP TABLE deposits;
DROP TABLE chain_cursors;
DROP TYPE refund_status;
DROP TYPE deposit_status;
