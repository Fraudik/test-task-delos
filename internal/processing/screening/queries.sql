-- name: ClaimAMLChecks :many
UPDATE deposit_aml_checks a
SET leased_until = now() + make_interval(secs => @lease_seconds::float8)
FROM deposits d
WHERE d.id = a.deposit_id
  AND a.deposit_id IN (
    SELECT c.deposit_id
    FROM deposit_aml_checks c
    JOIN deposits cd ON cd.id = c.deposit_id
    WHERE c.risk = ''
      AND c.next_attempt_at <= now()
      AND (c.leased_until IS NULL OR c.leased_until <= now())
      AND cd.status IN ('pending', 'confirmed')
    ORDER BY c.next_attempt_at, c.deposit_id
    LIMIT @max_rows
    FOR UPDATE OF c SKIP LOCKED
)
RETURNING a.deposit_id, d.tx_hash, d.from_address, d.to_address, d.amount, a.attempts;

-- name: RecordAMLResults :execrows
UPDATE deposit_aml_checks a
SET risk                 = r.risk,
    reason               = r.reason,
    next_attempt_at      = NULL,
    leased_until         = NULL,
    waiting_for_provider = false,
    checked_at           = now()
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@risks::text[]) AS risk, unnest(@reasons::text[]) AS reason) r
WHERE a.deposit_id = r.id AND a.risk = '';

-- name: ScheduleAMLRetries :exec
UPDATE deposit_aml_checks a
SET attempts             = a.attempts + 1,
    next_attempt_at      = now() + make_interval(secs => r.delay_seconds),
    leased_until         = NULL,
    waiting_for_provider = r.waiting_for_provider
FROM (
    SELECT unnest(@ids::bigint[])                    AS id,
           unnest(@delays_seconds::float8[])         AS delay_seconds,
           unnest(@waiting_for_provider::boolean[])  AS waiting_for_provider
) r
WHERE a.deposit_id = r.id AND a.risk = '';

-- name: ReleaseWaitingAMLChecks :execrows
UPDATE deposit_aml_checks
SET next_attempt_at = now()
WHERE risk = '' AND waiting_for_provider AND next_attempt_at > now();
