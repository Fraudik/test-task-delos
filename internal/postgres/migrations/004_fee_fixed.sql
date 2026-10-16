-- +goose Up
ALTER TABLE partners
    ADD COLUMN fee_fixed BIGINT NOT NULL DEFAULT 0 CHECK (fee_fixed >= 0);

ALTER TABLE deposits
    ADD COLUMN fee_fixed BIGINT NOT NULL DEFAULT 0 CHECK (fee_fixed >= 0);

-- +goose Down
ALTER TABLE deposits DROP COLUMN fee_fixed;
ALTER TABLE partners DROP COLUMN fee_fixed;
