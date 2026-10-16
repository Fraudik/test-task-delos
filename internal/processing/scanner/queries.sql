-- name: EnsureCursor :exec
INSERT INTO chain_cursors (name, next_block)
VALUES (@name, @next_block)
ON CONFLICT (name) DO NOTHING;

-- name: GetCursor :one
SELECT next_block
FROM chain_cursors
WHERE name = @name;

-- name: SaveCursor :exec
UPDATE chain_cursors
SET next_block = @next_block, updated_at = now()
WHERE name = @name;

-- name: StoredBlockHash :one
SELECT hash
FROM chain_blocks
WHERE cursor_name = @cursor_name AND number = @number;

-- name: StoredBlocksBelow :many
SELECT number, hash
FROM chain_blocks
WHERE cursor_name = @cursor_name AND number < @below
ORDER BY number DESC;

-- name: SaveBlocks :exec
INSERT INTO chain_blocks (cursor_name, number, hash)
SELECT @cursor_name, b.number, b.hash
FROM (
    SELECT unnest(@numbers::bigint[]) AS number,
           unnest(@hashes::text[])    AS hash
) b
ON CONFLICT (cursor_name, number) DO UPDATE SET hash = EXCLUDED.hash;

-- name: ForgetBlocksFrom :exec
DELETE FROM chain_blocks
WHERE cursor_name = @cursor_name AND number >= @from_block;

-- name: PruneBlocksBelow :exec
DELETE FROM chain_blocks
WHERE cursor_name = @cursor_name AND number < @below;

-- name: InvoicesByAddresses :many
SELECT i.id, i.partner_id, i.address
FROM invoices i
WHERE i.address = ANY (@addresses::text[]);

-- name: UpsertDeposit :one
INSERT INTO deposits (
    invoice_id, partner_id, tx_hash, log_index, block_number, block_hash,
    from_address, to_address, amount
)
VALUES (
    @invoice_id, @partner_id, @tx_hash, @log_index, @block_number, @block_hash,
    @from_address, @to_address, @amount
)
ON CONFLICT (tx_hash, log_index) DO UPDATE
SET block_number = EXCLUDED.block_number,
    block_hash   = EXCLUDED.block_hash,
    status       = 'pending',
    updated_at   = now()
WHERE deposits.status IN ('pending', 'orphaned')
  AND (deposits.status = 'orphaned' OR deposits.block_hash <> EXCLUDED.block_hash)
RETURNING id;

-- name: EnsureAMLCheck :exec
INSERT INTO deposit_aml_checks (deposit_id)
VALUES (@deposit_id)
ON CONFLICT (deposit_id) DO NOTHING;

-- name: PendingDepositsFrom :many
SELECT id, block_number, block_hash
FROM deposits
WHERE status = 'pending' AND block_number >= @from_block
ORDER BY block_number, id;

-- name: OrphanDeposit :execrows
UPDATE deposits
SET status = 'orphaned', updated_at = now()
WHERE id = @id AND status = 'pending' AND block_hash = @block_hash;
