-- +goose Up
CREATE TABLE chain_blocks (
    cursor_name TEXT NOT NULL REFERENCES chain_cursors (name) ON DELETE CASCADE,
    number      BIGINT NOT NULL CHECK (number >= 0),
    hash        TEXT NOT NULL CHECK (hash <> ''),
    PRIMARY KEY (cursor_name, number)
);

INSERT INTO chain_blocks (cursor_name, number, hash)
SELECT name, next_block - 1, last_hash
FROM chain_cursors
WHERE last_hash <> '' AND next_block > 0;

ALTER TABLE chain_cursors DROP COLUMN last_hash;

-- +goose Down
ALTER TABLE chain_cursors ADD COLUMN last_hash TEXT NOT NULL DEFAULT '';

UPDATE chain_cursors c
SET last_hash = b.hash
FROM chain_blocks b
WHERE b.cursor_name = c.name AND b.number = c.next_block - 1;

DROP TABLE chain_blocks;
