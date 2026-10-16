-- name: InsertTransactions :many
INSERT INTO ledger_transactions (idempotency_key, digest)
SELECT t.idempotency_key, t.digest
FROM (SELECT unnest(@idempotency_keys::text[]) AS idempotency_key, unnest(@digests::text[]) AS digest) t
ORDER BY t.idempotency_key
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id, idempotency_key;

-- name: GetTransactionDigests :many
SELECT idempotency_key, digest
FROM ledger_transactions
WHERE idempotency_key = ANY (@idempotency_keys::text[]);

-- name: EnsureAccounts :exec
INSERT INTO ledger_accounts (id, kind)
SELECT a.id, a.kind
FROM (SELECT unnest(@ids::text[]) AS id, unnest(@kinds::text[]) AS kind) a
ORDER BY a.id
ON CONFLICT (id) DO NOTHING;

-- name: LockAccounts :many
SELECT id, kind, balance
FROM ledger_accounts
WHERE id = ANY (@ids::text[])
ORDER BY id
FOR UPDATE;

-- name: ApplyAccountDeltas :exec
UPDATE ledger_accounts a
SET balance = a.balance + d.delta
FROM (SELECT unnest(@ids::text[]) AS id, unnest(@deltas::bigint[]) AS delta) d
WHERE a.id = d.id;

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
