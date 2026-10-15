CREATE TABLE IF NOT EXISTS node_explorer_service_migrations (
  version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS node_explorer_node_registry (
  identity text PRIMARY KEY, node jsonb NOT NULL, snapshot_id integer NOT NULL,
  node_index integer NOT NULL, historical_residential boolean NOT NULL DEFAULT false,
  available boolean NOT NULL DEFAULT true
);
CREATE TABLE IF NOT EXISTS node_explorer_connectivity (
  identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity),
  checked_at timestamptz NOT NULL, payload jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS node_explorer_exits (
  identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity),
  ip text NOT NULL, version bigint NOT NULL DEFAULT 1,
  checked_at timestamptz NOT NULL, payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS node_explorer_exits_ip ON node_explorer_exits(ip);
CREATE TABLE IF NOT EXISTS node_explorer_ip_attributes (
  ip text PRIMARY KEY, checked_at timestamptz NOT NULL, payload jsonb NOT NULL
);
-- Durable delivery ledger: Redis carries task IDs, never connection credentials.
-- This ledger repairs missed cross-store deliveries and fences expired workers.
CREATE TABLE IF NOT EXISTS node_explorer_work (
  id bigserial PRIMARY KEY, kind text NOT NULL, key text NOT NULL,
  generation bigint NOT NULL DEFAULT 1, priority integer NOT NULL DEFAULT 0,
  due_at timestamptz NOT NULL, payload jsonb NOT NULL DEFAULT '{}',
  state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','done')),
  lease_token text, lease_until timestamptz, attempts integer NOT NULL DEFAULT 0,
  error text NOT NULL DEFAULT '', UNIQUE(kind,key)
);
CREATE INDEX IF NOT EXISTS node_explorer_work_delivery ON node_explorer_work(kind,due_at) WHERE state <> 'done';
CREATE TABLE IF NOT EXISTS node_explorer_probe_results (
  session text NOT NULL, kind text NOT NULL, identity text NOT NULL,
  checked_at timestamptz NOT NULL, payload jsonb NOT NULL,
  PRIMARY KEY(session,kind)
);
CREATE TABLE IF NOT EXISTS node_explorer_service_status (
  service text PRIMARY KEY, updated_at timestamptz NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS node_explorer_provider_limits (
  provider text PRIMARY KEY, next_at timestamptz NOT NULL
);
CREATE OR REPLACE FUNCTION node_explorer_observation_changed() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
  UPDATE node_explorer_results_state SET source_revision = source_revision + 1;
  PERFORM pg_notify('node_explorer_changes', '');
  RETURN NULL;
END $$;
DO $$ DECLARE tbl text; BEGIN
  FOREACH tbl IN ARRAY ARRAY['node_explorer_connectivity','node_explorer_exits','node_explorer_ip_attributes'] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = tbl::regclass AND tgname = 'observation_changed') THEN
      EXECUTE format('CREATE TRIGGER observation_changed AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH STATEMENT EXECUTE FUNCTION node_explorer_observation_changed()', tbl);
    END IF;
  END LOOP;
END $$;
INSERT INTO node_explorer_service_migrations(version) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS node_explorer_lifecycle_events (
 id bigserial PRIMARY KEY, identity text NOT NULL, action text NOT NULL,
 dry_run boolean NOT NULL, reason text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);

DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='node_explorer_node_registry'::regclass AND tgname='registry_changed') THEN
  CREATE TRIGGER registry_changed AFTER INSERT OR UPDATE OR DELETE ON node_explorer_node_registry
  FOR EACH ROW EXECUTE FUNCTION node_explorer_observation_changed();
 END IF;
END $$;
