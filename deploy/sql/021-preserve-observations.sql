-- Stop liveness and exit workers before applying this migration. Existing
-- digests are rewritten only when they still match the old full-node digest.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
\ir ../../backend/internal/database/schema.sql
UPDATE node_explorer_connectivity c SET payload=jsonb_set(c.payload,'{configDigest}',to_jsonb(md5((n.node - 'rawLink' - 'generatedLink')::text)))
FROM node_explorer_node_registry n
WHERE c.identity=n.identity AND c.payload->>'configDigest'=md5(n.node::text);
DO $$ BEGIN
 IF to_regclass('node_explorer_liveness_attempts') IS NOT NULL THEN
  UPDATE node_explorer_liveness_attempts a SET digest=md5((n.node - 'rawLink' - 'generatedLink')::text)
  FROM node_explorer_node_registry n WHERE a.identity=n.identity AND a.digest=md5(n.node::text);
 END IF;
 IF to_regclass('node_explorer_exit_attempts') IS NOT NULL THEN
  UPDATE node_explorer_exit_attempts a SET digest=md5((n.node - 'rawLink' - 'generatedLink')::text)
  FROM node_explorer_node_registry n WHERE a.identity=n.identity AND a.digest=md5(n.node::text);
 END IF;
END $$;
INSERT INTO node_explorer_service_migrations(version) VALUES(21) ON CONFLICT DO NOTHING;
COMMIT;
