-- Preserve existing confirmed exit locations before deploying the new exit worker.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
\ir ../../backend/internal/database/schema.sql
UPDATE node_explorer_exits e SET pinned_country=upper(c.country)
FROM (SELECT e.identity, coalesce(
  CASE WHEN a.payload->>'ip'=e.ip THEN nullif(a.payload->>'country','') END,
  nullif(e.payload->>'country','')) AS country
  FROM node_explorer_exits e LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip) c
WHERE e.identity=c.identity AND e.pinned_country IS NULL AND c.country ~ '^[a-zA-Z]{2}$';
INSERT INTO node_explorer_service_migrations(version) VALUES(20) ON CONFLICT DO NOTHING;
COMMIT;
