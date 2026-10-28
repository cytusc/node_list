CREATE TABLE IF NOT EXISTS node_explorer_service_migrations (
  version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS node_explorer_node_registry (
  identity text PRIMARY KEY, node jsonb NOT NULL,
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
ALTER TABLE node_explorer_exits ADD COLUMN IF NOT EXISTS pinned_country text;
-- Raw current facts remain an ordinary view, so an identity restriction can
-- reach the base tables before any public node JSON is rendered.
CREATE OR REPLACE VIEW node_explorer_live_facts AS
WITH observed AS (
 SELECT n.identity,n.first_seen_at,n.node,c.payload AS connectivity,
 e.payload AS exit_payload,e.version AS exit_version,e.ip AS exit_ip,
 CASE WHEN e.identity IS NOT NULL AND e.ip<>'' AND a.payload->>'ip'=e.ip THEN a.payload END AS attributes,
 CASE WHEN e.identity IS NOT NULL THEN coalesce(e.pinned_country,
 CASE WHEN a.payload->>'ip'=e.ip THEN nullif(a.payload->>'country','') END,e.payload->>'country','') END AS observed_country
 FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_connectivity c USING(identity)
 LEFT JOIN node_explorer_exits e USING(identity)
 LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
 -- Publishing needs three independent facts, not just the scheduling flag:
 --   available     the last probe reached the target at all;
 --   connectivity a positive verdict for the current configuration/incarnation;
 --   c.checked_at  that verdict is recent (the worker refreshes every interval);
 --   verified      the exit could actually be observed through the node.
 -- registry.available deliberately stays the scheduling signal for the exit and
 -- attribute workers, so a hidden node keeps being probed and returns to the
 -- page on its own once it recovers.
 WHERE n.available
 AND c.payload->>'reachable'='true'
 AND c.payload->>'incarnation'=n.incarnation
 AND c.payload->>'configDigest'=md5(n.node::text)
 AND c.checked_at>now()-interval '6 hours'
 AND e.identity IS NOT NULL AND e.payload->>'verified'='true'
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
 SELECT f.*,
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
)
SELECT identity,country,
 node - 'rawLink' - 'generatedLink' - 'rawConfig' - 'ippure' - 'connectivity' - 'exit' - 'lifecycle'
 || jsonb_build_object('addedAt',first_seen_at,'lifecycle',jsonb_build_object('status','active'))
 || CASE WHEN connectivity IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('connectivity',connectivity) END
 || CASE WHEN exit IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('exit',exit,'ippure',ippure) END
 || CASE WHEN country IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('country',country) END
 || CASE WHEN coalesce(node->>'country','')<>'' AND upper(node->>'country')<>upper(coalesce(country,''))
 THEN jsonb_build_object('claimedCountry',node->>'country') ELSE '{}'::jsonb END AS payload
FROM facts;
