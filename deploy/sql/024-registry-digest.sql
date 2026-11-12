-- v1.5.113: store the node config digest on the registry so the liveness
-- scheduler and the live-facts view stop recomputing md5 over the whole
-- node JSON for every row on every query.
ALTER TABLE node_explorer_node_registry
  ADD COLUMN IF NOT EXISTS digest text
  GENERATED ALWAYS AS (md5((node - 'rawLink' - 'generatedLink')::text)) STORED;
