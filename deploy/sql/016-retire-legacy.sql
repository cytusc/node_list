-- Stop ALL old application writers and take a verified pg_dump before running.
-- Run with psql -v ON_ERROR_STOP=1; retained service facts are never rewritten.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
DROP VIEW IF EXISTS node_explorer_live_nodes;
DROP VIEW IF EXISTS node_explorer_live_facts;
DO $$ BEGIN
 IF to_regclass('node_explorer_snapshots') IS NOT NULL THEN
  DROP TRIGGER IF EXISTS results_changed ON node_explorer_snapshots;
 END IF;
END $$;
DROP TRIGGER IF EXISTS registry_changed ON node_explorer_node_registry;
DROP TRIGGER IF EXISTS observation_changed ON node_explorer_connectivity;
DROP TRIGGER IF EXISTS observation_changed ON node_explorer_exits;
DROP TRIGGER IF EXISTS observation_changed ON node_explorer_ip_attributes;
DROP TABLE IF EXISTS node_explorer_work;
DROP TABLE IF EXISTS node_explorer_probe_results;
DROP TABLE IF EXISTS node_explorer_provider_limits;
DROP TABLE IF EXISTS node_explorer_lifecycle_events;
DROP TABLE IF EXISTS node_explorer_deleted_nodes;
DROP TABLE IF EXISTS node_explorer_fast_results;
DROP TABLE IF EXISTS node_explorer_recovery_events;
DROP TABLE IF EXISTS node_explorer_recovery_totals;
DROP TABLE IF EXISTS node_explorer_node_links;
DROP TABLE IF EXISTS node_explorer_monitor_samples;
DROP TABLE IF EXISTS node_explorer_registry_batches;
DROP TABLE IF EXISTS node_explorer_ippure_results;
DROP TABLE IF EXISTS node_explorer_retired_nodes;
DROP TABLE IF EXISTS node_explorer_current_nodes;
DROP TABLE IF EXISTS node_explorer_results_changes;
DROP TABLE IF EXISTS node_explorer_results_state;
-- Historical one-off copies are already included in the full pre-migration backup.
DROP TABLE IF EXISTS node_explorer_firstseen_backup_20261025;
DROP TABLE IF EXISTS node_explorer_ip_type_clean_backup_archive;
DROP TABLE IF EXISTS zz_cleanup_attrs_20261023;
DROP TABLE IF EXISTS zz_cleanup_work_20261023;
DROP FUNCTION IF EXISTS node_explorer_results_changed();
DROP FUNCTION IF EXISTS node_explorer_observation_changed();
DROP INDEX IF EXISTS node_explorer_cleanup_due;
ALTER TABLE node_explorer_node_registry DROP COLUMN IF EXISTS historical_residential,
 DROP COLUMN IF EXISTS connection_key, DROP COLUMN IF EXISTS source_at;
DELETE FROM node_explorer_service_status WHERE service NOT IN
 ('api','source-ingest','node-liveness','exit-provision','exit-attributes');
\ir ../../backend/internal/database/schema.sql
INSERT INTO node_explorer_service_migrations(version) VALUES(16) ON CONFLICT DO NOTHING;
COMMIT;
