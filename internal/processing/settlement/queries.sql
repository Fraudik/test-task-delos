-- name: ClaimRefundableDeposits :many
SELECT d.id, d.partner_id, d.from_address, d.amount
FROM deposits d
JOIN deposit_aml_checks a ON a.deposit_id = d.id
WHERE d.status = 'confirmed' AND a.risk = 'high'
ORDER BY d.id
LIMIT @max_rows
FOR UPDATE OF d SKIP LOCKED;

-- name: ClaimPaidInvoices :many
SELECT i.id, i.partner_id, i.amount, i.fee_bps, i.fee_fixed
FROM invoices i
WHERE i.status = 'open'
  AND i.id IN (SELECT DISTINCT d.invoice_id FROM deposits d WHERE d.status = 'confirmed')
  AND i.amount <= (
    SELECT COALESCE(SUM(d.amount), 0)::bigint
    FROM deposits d
    JOIN deposit_aml_checks a ON a.deposit_id = d.id
    WHERE d.invoice_id = i.id AND d.status = 'confirmed' AND a.risk = 'low'
  )
ORDER BY i.id
LIMIT @max_rows
FOR UPDATE OF i SKIP LOCKED;

-- name: LockCleanDeposits :many
SELECT d.id, d.invoice_id, d.amount
FROM deposits d
JOIN deposit_aml_checks a ON a.deposit_id = d.id
WHERE d.invoice_id = ANY (@invoice_ids::bigint[]) AND d.status = 'confirmed' AND a.risk = 'low'
ORDER BY d.id
FOR UPDATE OF d;

-- name: FinishDeposits :execrows
UPDATE deposits
SET status = @status, updated_at = now()
WHERE id = ANY (@ids::bigint[]) AND status = 'confirmed';

-- name: MarkInvoicesPaid :execrows
UPDATE invoices
SET status = 'paid', paid_at = now()
WHERE id = ANY (@ids::bigint[]) AND status = 'open';

-- name: InsertRefunds :exec
INSERT INTO refunds (deposit_id, to_address, amount)
SELECT r.deposit_id, r.to_address, r.amount
FROM (
    SELECT unnest(@deposit_ids::bigint[]) AS deposit_id,
           unnest(@to_addresses::text[])  AS to_address,
           unnest(@amounts::bigint[])     AS amount
) r
ON CONFLICT (deposit_id) DO NOTHING;
