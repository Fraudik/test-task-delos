-- name: ClaimReadyDeposits :many
SELECT d.id, d.partner_id, d.amount
FROM deposits d
JOIN chain_cursors c ON c.name = @cursor_name
WHERE d.status = 'pending'
  AND d.block_number + @confirmations::bigint <= c.next_block
ORDER BY d.block_number, d.id
LIMIT @max_rows
FOR UPDATE OF d SKIP LOCKED;

-- name: ConfirmDeposits :execrows
UPDATE deposits
SET status = 'confirmed', updated_at = now()
WHERE id = ANY (@ids::bigint[]) AND status = 'pending';
