-- name: ClaimRefunds :many
UPDATE refunds f
SET initiate_next_attempt_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE f.id IN (
    SELECT c.id
    FROM refunds c
    WHERE c.initiated_at IS NULL AND c.initiate_next_attempt_at <= now()
    ORDER BY c.initiate_next_attempt_at, c.id
    LIMIT @max_rows
    FOR UPDATE SKIP LOCKED
)
RETURNING f.id, f.deposit_id, f.to_address, f.amount, f.initiate_attempts;

-- name: MarkRefundsInitiated :exec
UPDATE refunds
SET status = 'initiated', initiated_at = now()
WHERE id = ANY (@ids::bigint[]) AND initiated_at IS NULL;

-- name: ScheduleRefundRetries :exec
UPDATE refunds f
SET initiate_attempts        = f.initiate_attempts + 1,
    initiate_next_attempt_at = now() + make_interval(secs => r.delay_seconds)
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@delays_seconds::float8[]) AS delay_seconds) r
WHERE f.id = r.id AND f.initiated_at IS NULL;
