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
