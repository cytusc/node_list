-- Apply after updating the liveness service. Legacy evidence without a
-- configuration digest stays hidden until the service verifies it again.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
\ir ../../backend/internal/database/schema.sql
INSERT INTO node_explorer_service_migrations(version) VALUES(19) ON CONFLICT DO NOTHING;
COMMIT;
