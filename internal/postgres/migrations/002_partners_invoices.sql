-- +goose Up
CREATE TABLE partners (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL,
    fee_bps    INTEGER NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE invoices (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    partner_id BIGINT NOT NULL REFERENCES partners (id),
    address    TEXT NOT NULL UNIQUE CHECK (address = lower(address)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX invoices_partner_idx ON invoices (partner_id);

-- +goose Down
DROP TABLE invoices;
DROP TABLE partners;
