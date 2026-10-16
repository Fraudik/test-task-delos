-- name: EnsureCursor :exec
INSERT INTO chain_cursors (name, next_block)
VALUES (@name, @next_block)
ON CONFLICT (name) DO NOTHING;

-- name: GetCursor :one
SELECT next_block, last_hash
FROM chain_cursors
WHERE name = @name;

-- name: SaveCursor :exec
UPDATE chain_cursors
SET next_block = @next_block, last_hash = @last_hash, updated_at = now()
WHERE name = @name;

-- name: InvoicesByAddresses :many
SELECT i.id, i.partner_id, i.address, p.fee_bps, p.fee_fixed
FROM invoices i
JOIN partners p ON p.id = i.partner_id
WHERE i.address = ANY (@addresses::text[]);


-- name: UpsertDeposit :one
INSERT INTO deposits (
    invoice_id, partner_id, tx_hash, log_index, block_number, block_hash,
    from_address, to_address, amount, fee_bps, fee_fixed
)
VALUES (
    @invoice_id, @partner_id, @tx_hash, @log_index, @block_number, @block_hash,
    @from_address, @to_address, @amount, @fee_bps, @fee_fixed
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

-- name: ClaimAMLChecks :many
UPDATE deposit_aml_checks a
SET next_attempt_at = now() + make_interval(secs => @lease_seconds::float8)
FROM deposits d
WHERE d.id = a.deposit_id
  AND a.deposit_id IN (
    SELECT c.deposit_id
    FROM deposit_aml_checks c
    JOIN deposits cd ON cd.id = c.deposit_id
    WHERE c.risk = ''
      AND c.next_attempt_at <= now()
      AND cd.status IN ('pending', 'confirmed')
    ORDER BY c.next_attempt_at, c.deposit_id
    LIMIT @max_rows
    FOR UPDATE OF c SKIP LOCKED
)
RETURNING a.deposit_id, d.tx_hash, d.from_address, d.to_address, d.amount, a.attempts;

-- name: RecordAMLResults :execrows
UPDATE deposit_aml_checks a
SET risk            = r.risk,
    reason          = r.reason,
    next_attempt_at = NULL,
    checked_at      = now()
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@risks::text[]) AS risk, unnest(@reasons::text[]) AS reason) r
WHERE a.deposit_id = r.id AND a.risk = '';

-- name: ScheduleAMLRetries :exec
UPDATE deposit_aml_checks a
SET attempts        = a.attempts + 1,
    next_attempt_at = now() + make_interval(secs => r.delay_seconds)
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@delays_seconds::float8[]) AS delay_seconds) r
WHERE a.deposit_id = r.id AND a.risk = '';

-- name: ClaimSettleableDeposits :many
SELECT d.id, d.partner_id, d.from_address, d.amount, d.fee_bps, d.fee_fixed, a.risk AS aml_risk
FROM deposits d
JOIN deposit_aml_checks a ON a.deposit_id = d.id
WHERE d.status = 'confirmed' AND a.risk <> ''
ORDER BY d.id
LIMIT @max_rows
FOR UPDATE OF d SKIP LOCKED;

-- name: FinishDeposits :execrows
UPDATE deposits d
SET status = f.status::deposit_status, updated_at = now()
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@statuses::text[]) AS status) f
WHERE d.id = f.id AND d.status = 'confirmed';

-- name: InsertRefunds :exec
INSERT INTO refunds (deposit_id, to_address, amount)
SELECT r.deposit_id, r.to_address, r.amount
FROM (
    SELECT unnest(@deposit_ids::bigint[]) AS deposit_id,
           unnest(@to_addresses::text[])  AS to_address,
           unnest(@amounts::bigint[])     AS amount
) r
ON CONFLICT (deposit_id) DO NOTHING;

-- name: FlagForReview :execrows
UPDATE deposits d
SET status        = 'needs_review',
    review_reason = r.reason,
    flagged_at    = now(),
    updated_at    = now()
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@reasons::text[]) AS reason) r
WHERE d.id = r.id AND d.status IN ('pending', 'confirmed');
