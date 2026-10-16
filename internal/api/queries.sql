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
INSERT INTO invoices (partner_id, address, idempotency_key)
VALUES (@partner_id, @address, @idempotency_key::text)
ON CONFLICT (partner_id, idempotency_key) DO NOTHING
RETURNING id, address, created_at;

-- name: InvoiceByIdempotencyKey :one
SELECT id, address, created_at
FROM invoices
WHERE partner_id = @partner_id AND idempotency_key = @idempotency_key::text;

-- name: GetInvoice :one
SELECT id, address, created_at
FROM invoices
WHERE id = @id AND partner_id = @partner_id;

-- name: InvoiceDeposits :many
SELECT id, tx_hash, log_index, block_number, block_hash, amount, status, created_at
FROM deposits
WHERE invoice_id = @invoice_id
ORDER BY id;

-- name: RewindCursor :execrows
UPDATE chain_cursors
SET next_block = @next_block, last_hash = '', updated_at = now()
WHERE name = @name AND next_block > @next_block;
