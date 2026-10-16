-- name: ClaimNotifications :many
UPDATE invoices i
SET notify_next_attempt_at = now() + make_interval(secs => @lease_seconds::float8)
WHERE i.id IN (
    SELECT c.id
    FROM invoices c
    WHERE c.status = 'paid' AND c.notified_at IS NULL AND c.notify_next_attempt_at <= now()
    ORDER BY c.notify_next_attempt_at, c.id
    LIMIT @max_rows
    FOR UPDATE SKIP LOCKED
)
RETURNING i.id, i.partner_id, i.amount, i.notify_attempts,
    (SELECT COALESCE(SUM(d.amount), 0)::bigint FROM deposits d WHERE d.invoice_id = i.id AND d.status = 'credited') AS credited;

-- name: MarkNotified :exec
UPDATE invoices
SET notified_at = now()
WHERE id = ANY (@ids::bigint[]) AND notified_at IS NULL;

-- name: ScheduleNotifyRetries :exec
UPDATE invoices i
SET notify_attempts        = i.notify_attempts + 1,
    notify_next_attempt_at = now() + make_interval(secs => r.delay_seconds)
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@delays_seconds::float8[]) AS delay_seconds) r
WHERE i.id = r.id AND i.notified_at IS NULL;
