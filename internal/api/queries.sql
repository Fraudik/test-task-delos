-- name: UpsertPartner :one
INSERT INTO partners (name, fee_bps, fee_fixed, api_key_hash)
VALUES (@name, @fee_bps, @fee_fixed, @api_key_hash::text)
ON CONFLICT (name) DO UPDATE
SET fee_bps      = EXCLUDED.fee_bps,
    fee_fixed    = EXCLUDED.fee_fixed,
    api_key_hash = EXCLUDED.api_key_hash
RETURNING id;

-- name: PartnerByAPIKeyHash :one
SELECT id, name
FROM partners
WHERE api_key_hash = @api_key_hash::text;

-- name: CreateInvoice :one
INSERT INTO invoices (partner_id, address, idempotency_key, amount, fee_bps, fee_fixed)
SELECT p.id, @address, @idempotency_key::text, @amount, p.fee_bps, p.fee_fixed
FROM partners p
WHERE p.id = @partner_id
ON CONFLICT (partner_id, idempotency_key) DO NOTHING
RETURNING id, address, amount, status, created_at;

-- name: InvoiceByIdempotencyKey :one
SELECT id, address, amount, status, created_at
FROM invoices
WHERE partner_id = @partner_id AND idempotency_key = @idempotency_key::text;

-- name: GetInvoice :one
SELECT id, address, amount, status, created_at
FROM invoices
WHERE id = @id AND partner_id = @partner_id;

-- name: InvoiceDeposits :many
SELECT d.id, d.tx_hash, d.log_index, d.block_number, d.block_hash, d.amount, d.status, d.created_at,
    COALESCE(a.risk, '')::text AS aml_risk
FROM deposits d
LEFT JOIN deposit_aml_checks a ON a.deposit_id = d.id
WHERE d.invoice_id = @invoice_id
ORDER BY d.id;

-- name: RewindCursor :one
WITH rewound AS (
    UPDATE chain_cursors
    SET next_block = @next_block, updated_at = now()
    WHERE name = @name AND next_block > @next_block
    RETURNING name
), forgotten AS (
    DELETE FROM chain_blocks b
    USING rewound r
    WHERE b.cursor_name = r.name AND b.number >= @next_block
)
SELECT count(*) FROM rewound;
