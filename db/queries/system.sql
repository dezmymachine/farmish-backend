-- name: ListExtensions :many
-- Installed Postgres extensions; used by tests to assert migration 000001.
SELECT extname::text AS name
FROM pg_catalog.pg_extension
ORDER BY extname;
