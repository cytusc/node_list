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
-- Select the newest reachable control node without scanning and sorting all proofs.
CREATE INDEX IF NOT EXISTS node_explorer_connectivity_reference
  ON node_explorer_connectivity (checked_at DESC,identity)
  WHERE payload->>'reachable'='true';
-- JSON expression statistics let the planner choose the ordered index for LIMIT 1.
CREATE STATISTICS IF NOT EXISTS node_explorer_connectivity_reachable
  ON (payload->>'reachable') FROM node_explorer_connectivity;
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
-- The cleanup veto and the admission busy-check both filter live work by the
-- job's identity; without this every verification task scans the whole ledger.
-- Full, not partial: the cleanup DELETE filters only on identity, so a partial
-- index would be unusable and every removal would seq-scan the whole ledger
-- (measured 670 ms against 108k rows / 1.2 GB).
CREATE INDEX IF NOT EXISTS node_explorer_work_identity ON node_explorer_work((payload->>'identity'));
-- The deletion gate checks the survival-set size on every removal; a partial
-- index keeps that check cheap once the registry is large again.
CREATE INDEX IF NOT EXISTS node_explorer_registry_available ON node_explorer_node_registry(identity) WHERE available;
CREATE TABLE IF NOT EXISTS node_explorer_probe_results (
  session text NOT NULL, kind text NOT NULL, identity text NOT NULL,
  checked_at timestamptz NOT NULL, payload jsonb NOT NULL,
  PRIMARY KEY(session,kind)
);
CREATE TABLE IF NOT EXISTS node_explorer_service_status (
  service text PRIMARY KEY, updated_at timestamptz NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'
);
-- Bounded, credential-free monitoring history. Written once per minute by
-- source-ingest, independently of browser visits; no per-probe write hotspot.
CREATE TABLE IF NOT EXISTS node_explorer_monitor_samples (
  sampled_at timestamptz PRIMARY KEY,
  payload jsonb NOT NULL
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

-- Latest delay observations are retained separately; only fenced 204 success becomes connectivity evidence.
CREATE TABLE IF NOT EXISTS node_explorer_fast_results (
 identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity) ON DELETE CASCADE,
 checked_at timestamptz NOT NULL, payload jsonb NOT NULL
);

-- Session deadline checks are frequent and must not scan historical work JSON.
CREATE INDEX IF NOT EXISTS node_explorer_work_capacity_session ON node_explorer_work
 ((payload->>'session'),kind) WHERE payload ? 'capacityAt';
-- Admission counts include queued coordinators but only the bounded in-flight set.
CREATE INDEX IF NOT EXISTS node_explorer_work_admitted ON node_explorer_work (kind,priority)
 WHERE state<>'done' AND payload ? 'startedAt' AND payload->>'startedAt' NOT LIKE '0001-%';

-- Assessment outcomes outlive the mutable delivery ledger and its 16-item history.
-- No FK: deleting a node or its work must not erase throughput statistics.
CREATE TABLE IF NOT EXISTS node_explorer_recovery_events (
  work_id bigint NOT NULL, checked_at timestamptz NOT NULL,
  kind text NOT NULL, result text NOT NULL,
  PRIMARY KEY (work_id,checked_at)
);
CREATE INDEX IF NOT EXISTS node_explorer_recovery_events_time
  ON node_explorer_recovery_events (checked_at,kind);
CREATE TABLE IF NOT EXISTS node_explorer_recovery_totals (
  kind text NOT NULL, result text NOT NULL, completed bigint NOT NULL,
  PRIMARY KEY (kind,result)
);
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM node_explorer_service_migrations WHERE version=7) THEN
    -- Import only observed outcomes. Older truncated history cannot be invented.
    INSERT INTO node_explorer_recovery_events(work_id,checked_at,kind,result)
    SELECT w.id,(h->>'checkedAt')::timestamptz,w.kind,h->>'result'
    FROM node_explorer_work w
    CROSS JOIN LATERAL jsonb_array_elements(CASE
      WHEN jsonb_array_length(coalesce(w.payload->'recoveryHistory','[]'))>0
      THEN w.payload->'recoveryHistory'
      ELSE jsonb_build_array(jsonb_build_object('checkedAt',w.payload->>'checkedAt','result',w.payload->>'recoveryResult'))
    END) h
    WHERE w.kind IN ('recovery','normal-recovery')
      AND coalesce(h->>'checkedAt','')<>'' AND h->>'checkedAt' NOT LIKE '0001-%'
      AND coalesce(h->>'result','')<>''
      AND h->>'result'<>'cancelled-historical-backlog'
    ON CONFLICT DO NOTHING;
    INSERT INTO node_explorer_recovery_totals(kind,result,completed)
    SELECT kind,result,count(*) FROM node_explorer_recovery_events GROUP BY kind,result
    ON CONFLICT(kind,result) DO UPDATE SET completed=EXCLUDED.completed;
    DELETE FROM node_explorer_recovery_events WHERE checked_at<now()-interval '2 hours';
  END IF;
END $$;
INSERT INTO node_explorer_service_migrations(version) VALUES (7) ON CONFLICT DO NOTHING;

-- Recovery proofs expire quickly; find their queued exit dependencies without
-- sorting the periodic exit backlog. Claim retains all normal lease fences.
CREATE INDEX IF NOT EXISTS node_explorer_work_recovery_exit
 ON node_explorer_work ((priority>0),due_at,id)
 WHERE kind='exit' AND state='queued' AND payload->>'session' LIKE 'recovery:%';

-- Retire the superseded cleanup coordinator and only its probe dependencies.
-- Incrementing generations and clearing leases fences in-flight Redis deliveries.
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM node_explorer_service_migrations WHERE version=13) THEN
  UPDATE node_explorer_work SET state='done',generation=generation+1,
   lease_token=NULL,lease_until=NULL,published_at=NULL,error='retired: dead-verify owns deletion'
  WHERE kind='node-cleanup' OR payload->>'action'='cleanup'
   OR (kind IN ('fast-delay','connectivity','exit') AND payload->>'session' LIKE 'cleanup:%');
  DELETE FROM node_explorer_probe_results WHERE session LIKE 'cleanup:%';
  DELETE FROM node_explorer_service_status WHERE service IN ('node-cleanup','lifecycle');
 END IF;
END $$;
INSERT INTO node_explorer_service_migrations(version) VALUES(13) ON CONFLICT DO NOTHING;

-- Link provenance is independent of compacted snapshots and node lifecycles.
-- Never cascade-delete it when a source, snapshot or registry node is pruned.
-- origin distinguishes upstream bytes from config-derived fallbacks.
CREATE TABLE IF NOT EXISTS node_explorer_node_links (
 id text PRIMARY KEY, connection_key text NOT NULL, link text NOT NULL,
 origin text NOT NULL CHECK(origin IN ('original','generated')),
 first_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS node_explorer_node_links_connection
 ON node_explorer_node_links(connection_key,origin,first_seen_at,id);
