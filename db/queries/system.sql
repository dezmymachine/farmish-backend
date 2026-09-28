-- name: ListExtensions :many
-- Installed Postgres extensions; used by tests to assert migration 000001.
SELECT extname::text AS name
FROM pg_catalog.pg_extension
ORDER BY extname;

-- name: CountJobsByState :many
-- Feeds the jobs_queue_depth metric (Phase 21). River's state column is an
-- enum, so cast it for a plain string in Go.
SELECT state::text AS state, count(*)::bigint AS jobs
FROM river_job
GROUP BY state;
