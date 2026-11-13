-- v1.5.120: expose the importer source (subs-check, qilan, proxio-all,
-- fmw86-socks5, ...) as a first-class facet/filter column on the live-facts
-- view. The value is the node's stored subTag: provenance, not an observed
-- fact, so it is read directly and never reclassified. Re-applying schema.sql
-- replaces both live views, which is how the new column reaches the API.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
\ir ../../backend/internal/database/schema.sql
INSERT INTO node_explorer_service_migrations(version) VALUES(25) ON CONFLICT DO NOTHING;
COMMIT;
