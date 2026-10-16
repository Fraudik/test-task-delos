-- +goose Up
ALTER TABLE partners
    ADD COLUMN api_key_hash TEXT,
    ADD CONSTRAINT partners_name_key UNIQUE (name),
    ADD CONSTRAINT partners_api_key_hash_key UNIQUE (api_key_hash);

ALTER TABLE invoices
    ADD COLUMN idempotency_key TEXT CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    ADD CONSTRAINT invoices_partner_idempotency_key UNIQUE (partner_id, idempotency_key);

-- +goose Down
ALTER TABLE invoices
    DROP CONSTRAINT invoices_partner_idempotency_key,
    DROP COLUMN idempotency_key;
ALTER TABLE partners
    DROP CONSTRAINT partners_api_key_hash_key,
    DROP CONSTRAINT partners_name_key,
    DROP COLUMN api_key_hash;
