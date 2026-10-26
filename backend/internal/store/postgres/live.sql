-- A regular view: evaluated only for a request, with no rebuild or copied rows.
CREATE COLLATION IF NOT EXISTS node_explorer_numeric
  (provider = icu, locale = 'zh-u-kn-true');

ALTER TABLE node_explorer_node_registry ADD COLUMN IF NOT EXISTS connection_key text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS node_explorer_registry_connection_key ON node_explorer_node_registry(connection_key) WHERE available AND connection_key<>'';

ALTER TABLE node_explorer_node_registry ADD COLUMN IF NOT EXISTS source_at timestamptz NOT NULL DEFAULT '-infinity';
CREATE TABLE IF NOT EXISTS node_explorer_registry_batches (
 snapshot_id integer PRIMARY KEY, revision bigint NOT NULL
);
-- Source membership avoids decoding the entire snapshot history per deletion.
CREATE TABLE IF NOT EXISTS node_explorer_snapshot_members (
 snapshot_id integer NOT NULL REFERENCES node_explorer_snapshots(id) ON DELETE CASCADE,
 node_index integer NOT NULL, identity text NOT NULL,
 PRIMARY KEY(snapshot_id,node_index)
);
CREATE INDEX IF NOT EXISTS node_explorer_snapshot_members_identity ON node_explorer_snapshot_members(identity,snapshot_id);
-- A schema migration runs once; existing cursors need a membership backfill.
DELETE FROM node_explorer_registry_batches;
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
 WHERE n.available AND NOT EXISTS(SELECT 1 FROM node_explorer_retired_nodes r WHERE r.identity=n.identity)
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
