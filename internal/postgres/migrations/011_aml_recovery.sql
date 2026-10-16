-- +goose Up
ALTER TABLE deposit_aml_checks
    ADD COLUMN leased_until         TIMESTAMPTZ,
    ADD COLUMN waiting_for_provider BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX deposit_aml_checks_waiting_idx ON deposit_aml_checks (deposit_id)
    WHERE risk = '' AND waiting_for_provider;

-- +goose Down
DROP INDEX deposit_aml_checks_waiting_idx;

ALTER TABLE deposit_aml_checks
    DROP COLUMN waiting_for_provider,
    DROP COLUMN leased_until;
