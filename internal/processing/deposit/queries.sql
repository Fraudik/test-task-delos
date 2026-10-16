-- name: FlagForReview :execrows
UPDATE deposits d
SET status        = 'needs_review',
    review_reason = r.reason,
    flagged_at    = now(),
    updated_at    = now()
FROM (SELECT unnest(@ids::bigint[]) AS id, unnest(@reasons::text[]) AS reason) r
WHERE d.id = r.id AND d.status IN ('pending', 'confirmed');
