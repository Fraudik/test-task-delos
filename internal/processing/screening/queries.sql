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
