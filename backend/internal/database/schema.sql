CREATE TABLE IF NOT EXISTS node_explorer_snapshots (
  id SERIAL PRIMARY KEY,
  fingerprint TEXT NOT NULL UNIQUE,
  checked_at TEXT,
  imported_at TEXT NOT NULL,
  node_count INTEGER NOT NULL,
  payload TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS node_explorer_service_migrations (
  version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS node_explorer_node_registry (
  identity text PRIMARY KEY, node jsonb NOT NULL, snapshot_id integer NOT NULL,
  node_index integer NOT NULL,
  available boolean NOT NULL DEFAULT false
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
CREATE TABLE IF NOT EXISTS node_explorer_ip_attributes (
  ip text PRIMARY KEY, checked_at timestamptz NOT NULL, payload jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS node_explorer_service_status (
  service text PRIMARY KEY, updated_at timestamptz NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS node_explorer_connectivity_reference
  ON node_explorer_connectivity (checked_at DESC,identity)
  WHERE payload->>'reachable'='true';
CREATE INDEX IF NOT EXISTS node_explorer_exits_ip ON node_explorer_exits(ip);
CREATE INDEX IF NOT EXISTS node_explorer_registry_available ON node_explorer_node_registry(identity) WHERE available;
ALTER TABLE node_explorer_node_registry ADD COLUMN IF NOT EXISTS first_seen_at timestamptz NOT NULL DEFAULT now(), ADD COLUMN IF NOT EXISTS last_available_at timestamptz, ADD COLUMN IF NOT EXISTS incarnation text NOT NULL DEFAULT md5(random()::text || clock_timestamp()::text);
-- A regular view: evaluated only for a request, with no rebuild or copied rows.
CREATE COLLATION IF NOT EXISTS node_explorer_numeric
  (provider = icu, locale = 'zh-u-kn-true');


-- Source membership locates the exact input used by liveness.
CREATE TABLE IF NOT EXISTS node_explorer_snapshot_members (
 snapshot_id integer NOT NULL REFERENCES node_explorer_snapshots(id) ON DELETE CASCADE,
 node_index integer NOT NULL, identity text NOT NULL,
 PRIMARY KEY(snapshot_id,node_index)
);
CREATE INDEX IF NOT EXISTS node_explorer_snapshot_members_identity ON node_explorer_snapshot_members(identity,snapshot_id);
-- Raw current facts remain an ordinary view, so an identity restriction can
-- reach the base tables before any public node JSON is rendered.
CREATE OR REPLACE VIEW node_explorer_live_facts AS
WITH observed AS (
 SELECT n.identity,n.snapshot_id,n.node_index,n.node,c.payload AS connectivity,
 e.payload AS exit_payload,e.version AS exit_version,e.ip AS exit_ip,
 CASE WHEN e.identity IS NOT NULL AND e.ip<>'' AND a.payload->>'ip'=e.ip THEN a.payload END AS attributes,
 CASE WHEN e.identity IS NOT NULL THEN coalesce(
 CASE WHEN a.payload->>'ip'=e.ip THEN nullif(a.payload->>'country','') END,e.payload->>'country','') END AS observed_country
 FROM node_explorer_node_registry n
 JOIN node_explorer_snapshots s ON s.id=n.snapshot_id
 LEFT JOIN node_explorer_connectivity c USING(identity)
 LEFT JOIN node_explorer_exits e USING(identity)
 LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
 WHERE n.available
)
SELECT observed.*,
 CASE WHEN observed_country ~ '^[a-zA-Z]{2}$' THEN
 CASE WHEN upper(coalesce(node->>'country',''))=upper(observed_country) THEN node->>'country'
 ELSE upper(observed_country) END ELSE node->>'country' END AS country,
 CASE WHEN exit_payload IS NULL THEN NULL
 WHEN exit_ip<>'' AND exit_payload->>'addressFamily'='6' THEN 'unsupported'
 WHEN attributes IS NOT NULL THEN attributes->>'status' ELSE 'error' END AS ip_status
FROM observed;

CREATE OR REPLACE VIEW node_explorer_live_nodes AS
WITH facts AS (
 SELECT f.*, n.first_seen_at,
 exit_payload || jsonb_build_object('version',exit_version) AS exit,
 CASE WHEN exit_payload IS NULL THEN NULL
 ELSE coalesce(attributes,jsonb_build_object('status','error','ip',exit_ip,'checkedAt',exit_payload->>'checkedAt',
 'country',exit_payload->>'country','attempts',0,
 'failureStage',CASE WHEN exit_ip<>'' THEN 'ippure-query' ELSE 'exit-ip' END,
 'error',CASE WHEN exit_ip<>'' THEN 'IP 属性待查询' ELSE coalesce(exit_payload->>'error','') END))
 || jsonb_build_object('exitIPSource',exit_payload->>'source','exitIPMode','proxy-echo',
 'exitIPVersion',3,'addressFamily',exit_payload->'addressFamily','country',observed_country)
 || CASE WHEN exit_ip<>'' AND exit_payload->>'addressFamily'='6' THEN
 jsonb_build_object('status','unsupported','failureStage','unsupported-ipv6','error','仅 IPv6，暂无评分')
 ELSE '{}'::jsonb END END AS ippure
 FROM node_explorer_live_facts f
 JOIN node_explorer_node_registry n USING(identity)
)
SELECT identity,snapshot_id,node_index,country,
 node - 'rawLink' - 'generatedLink' - 'rawConfig' - 'ippure' - 'connectivity' - 'exit' - 'lifecycle'
 || jsonb_build_object('addedAt',first_seen_at,'lifecycle',jsonb_build_object('status','active'))
 || CASE WHEN connectivity IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('connectivity',connectivity) END
 || CASE WHEN exit IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('exit',exit,'ippure',ippure) END
 || CASE WHEN country IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('country',country) END
 || CASE WHEN coalesce(node->>'country','')<>'' AND upper(node->>'country')<>upper(coalesce(country,''))
 THEN jsonb_build_object('claimedCountry',node->>'country') ELSE '{}'::jsonb END AS payload
FROM facts;
