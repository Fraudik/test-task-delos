-- name: EnsureCursor :exec
INSERT INTO chain_cursors (name, next_block)
VALUES (@name, @next_block)
ON CONFLICT (name) DO NOTHING;

-- name: LockCursor :one
SELECT next_block, last_hash
FROM chain_cursors
WHERE name = @name;

-- name: SaveCursor :exec
UPDATE chain_cursors
SET next_block = @next_block, last_hash = @last_hash, updated_at = now()
WHERE name = @name;

-- name: InvoicesByAddresses :many
SELECT i.id, i.partner_id, i.address, p.fee_bps
FROM invoices i
JOIN partners p ON p.id = i.partner_id
WHERE i.address = ANY (@addresses::text[]);

-- name: UpsertDeposit :execrows
INSERT INTO deposits (
    invoice_id, partner_id, tx_hash, log_index, block_number, block_hash,
    from_address, to_address, amount, fee_bps
)
VALUES (
    @invoice_id, @partner_id, @tx_hash, @log_index, @block_number, @block_hash,
    @from_address, @to_address, @amount, @fee_bps
)
ON CONFLICT (tx_hash, log_index) DO UPDATE
SET block_number = EXCLUDED.block_number,
    block_hash   = EXCLUDED.block_hash,
    status       = 'pending',
    updated_at   = now()
WHERE deposits.status IN ('pending', 'orphaned')
  AND (deposits.status = 'orphaned' OR deposits.block_hash <> EXCLUDED.block_hash);