-- +goose Up
ALTER TYPE deposit_status ADD VALUE 'needs_review';

ALTER TABLE deposits
    ADD COLUMN review_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN flagged_at    TIMESTAMPTZ,
    ADD CONSTRAINT deposits_needs_review_check
        CHECK ((status::text = 'needs_review') = (flagged_at IS NOT NULL AND review_reason <> ''));

CREATE INDEX deposits_needs_review_idx ON deposits (flagged_at) WHERE flagged_at IS NOT NULL;

-- +goose Down
DROP INDEX deposits_needs_review_idx;
ALTER TABLE deposits
    DROP CONSTRAINT deposits_needs_review_check,
    DROP COLUMN flagged_at,
    DROP COLUMN review_reason;
