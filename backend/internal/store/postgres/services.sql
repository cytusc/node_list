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
-- Live readers no longer consume projection revisions or notifications for
-- runtime observations. Remove the old global-row write hotspot.
DROP TRIGGER IF EXISTS registry_changed ON node_explorer_node_registry;
DROP TRIGGER IF EXISTS observation_changed ON node_explorer_connectivity;
DROP TRIGGER IF EXISTS observation_changed ON node_explorer_exits;
DROP TRIGGER IF EXISTS observation_changed ON node_explorer_ip_attributes;
DROP FUNCTION IF EXISTS node_explorer_observation_changed();

ALTER TABLE node_explorer_work
 ADD COLUMN IF NOT EXISTS published_at timestamptz,
 ADD COLUMN IF NOT EXISTS published_generation bigint,
 ADD COLUMN IF NOT EXISTS published_due_at timestamptz,
 ADD COLUMN IF NOT EXISTS published_priority integer;
INSERT INTO node_explorer_service_migrations(version) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS node_explorer_lifecycle_events (
 id bigserial PRIMARY KEY, identity text NOT NULL, action text NOT NULL,
 dry_run boolean NOT NULL, reason text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);

-- Migration time is a conservative observation baseline for legacy/unknown nodes.
ALTER TABLE node_explorer_node_registry
 ADD COLUMN IF NOT EXISTS first_seen_at timestamptz NOT NULL DEFAULT now(),
 ADD COLUMN IF NOT EXISTS last_available_at timestamptz,
 ADD COLUMN IF NOT EXISTS incarnation text NOT NULL DEFAULT md5(random()::text || clock_timestamp()::text);
UPDATE node_explorer_node_registry n SET last_available_at=c.checked_at
 FROM node_explorer_connectivity c WHERE c.identity=n.identity AND n.last_available_at IS NULL
 AND c.payload->>'reachable'='true' AND coalesce(c.payload->>'error','')='';
CREATE INDEX IF NOT EXISTS node_explorer_cleanup_due ON node_explorer_node_registry
 (greatest(first_seen_at,coalesce(last_available_at,first_seen_at)));
-- Only connection identities and a watermark remain after physical deletion.
CREATE TABLE IF NOT EXISTS node_explorer_deleted_nodes (
 identity text PRIMARY KEY, deleted_at timestamptz NOT NULL
);
