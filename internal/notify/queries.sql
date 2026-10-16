-- name: ClaimNotifications :many
UPDATE deposits d
SET notify_next_attempt_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE d.id IN (
    SELECT c.id
    FROM deposits c
    WHERE c.status = 'credited' AND c.notified_at IS NULL AND c.notify_next_attempt_at <= now()
    ORDER BY c.notify_next_attempt_at, c.id
    LIMIT @max_rows
    FOR UPDATE SKIP LOCKED
)
RETURNING d.id, d.invoice_id, d.partner_id, d.tx_hash, d.amount, d.notify_attempts;

-- name: MarkNotified :exec
UPDATE deposits
SET notified_at = now()
WHERE id = ANY (@ids::bigint[]) AND notified_at IS NULL;

-- name: ScheduleNotifyRetries :exec
UPDATE deposits d
SET notify_attempts        = d.notify_attempts + 1,
    notify_next_attempt_at = now() + make_interval(secs => r.delay_seconds)
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@delays_seconds::float8[]) AS delay_seconds) r
WHERE d.id = r.id AND d.notified_at IS NULL;
