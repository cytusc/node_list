-- Add the shared replay-protection table. Safe to apply repeatedly.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));
CREATE TABLE IF NOT EXISTS node_explorer_open_api_nonces (
  app_id varchar(64) NOT NULL, nonce varchar(128) NOT NULL,
  expires_at bigint NOT NULL, PRIMARY KEY (app_id,nonce)
);
CREATE INDEX IF NOT EXISTS node_explorer_open_api_nonces_expiry
  ON node_explorer_open_api_nonces(expires_at);
INSERT INTO node_explorer_service_migrations(version) VALUES(23) ON CONFLICT DO NOTHING;
COMMIT;
