-- name: InsertTransaction :one
INSERT INTO ledger_transactions (idempotency_key, digest)
VALUES (@idempotency_key, @digest)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id;

-- name: GetTransactionDigest :one
SELECT digest
FROM ledger_transactions
WHERE idempotency_key = @idempotency_key;

-- name: EnsureAccount :exec
INSERT INTO ledger_accounts (id, kind)
VALUES (@id, @kind)
ON CONFLICT (id) DO NOTHING;

-- name: ApplyAccountDelta :execrows
UPDATE ledger_accounts
SET balance = balance + @delta
WHERE id = @id AND kind = @kind;

-- name: InsertEntries :copyfrom
INSERT INTO ledger_entries (transaction_id, account_id, amount)
VALUES (@transaction_id, @account_id, @amount);

-- name: GetAccountBalance :one
SELECT balance
FROM ledger_accounts
WHERE id = @id;

-- name: IntegrityReport :one
SELECT
    (SELECT COALESCE(SUM(balance), 0) FROM ledger_accounts)::bigint AS total,
    (SELECT count(*)
     FROM ledger_accounts a
     LEFT JOIN (SELECT account_id, SUM(amount) AS s FROM ledger_entries GROUP BY account_id) e
         ON e.account_id = a.id
     WHERE a.balance <> COALESCE(e.s, 0))::bigint AS drifted_accounts,
    (SELECT count(*)
     FROM (SELECT 1 FROM ledger_entries GROUP BY transaction_id HAVING SUM(amount) <> 0) t)::bigint AS unbalanced_transactions;
