-- Stop application containers and take a verified pg_dump before running.
-- Deploy the matching application version before restarting any writer.
-- Run with psql -v ON_ERROR_STOP=1 -f deploy/sql/017-remove-snapshots.sql.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
DROP VIEW IF EXISTS node_explorer_live_nodes;
DROP VIEW IF EXISTS node_explorer_live_facts;
ALTER TABLE node_explorer_node_registry DROP COLUMN IF EXISTS snapshot_id,
 DROP COLUMN IF EXISTS node_index;
ALTER TABLE IF EXISTS node_explorer_source_batches DROP COLUMN IF EXISTS snapshot_id;
DROP TABLE IF EXISTS node_explorer_snapshot_members;
DROP TABLE IF EXISTS node_explorer_snapshots;
DROP COLLATION IF EXISTS node_explorer_numeric;
\ir ../../backend/internal/database/schema.sql
INSERT INTO node_explorer_service_migrations(version) VALUES(17) ON CONFLICT DO NOTHING;
COMMIT;
