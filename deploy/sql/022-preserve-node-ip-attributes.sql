-- Keep each node's last successful IP classification when its exit IP changes.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
\ir ../../backend/internal/database/schema.sql
UPDATE node_explorer_exits e SET pinned_attributes=a.payload
FROM node_explorer_ip_attributes a
WHERE a.ip=e.ip AND a.payload->>'ip'=e.ip
 AND (a.payload->>'status'='success' OR jsonb_array_length(coalesce(a.payload->'classification'->'types','[]'::jsonb))>0)
 AND e.pinned_attributes IS NULL;
INSERT INTO node_explorer_service_migrations(version) VALUES(22) ON CONFLICT DO NOTHING;
COMMIT;
