CREATE TABLE IF NOT EXISTS node_explorer_source_connections (
 version text NOT NULL, fingerprint text NOT NULL, canonical text NOT NULL,
 identity text NOT NULL REFERENCES node_explorer_node_registry(identity),
 first_seen_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(version,fingerprint), UNIQUE(identity)
);
CREATE TABLE IF NOT EXISTS node_explorer_source_batches (
 source text NOT NULL, digest text NOT NULL,
 version text NOT NULL, status text NOT NULL CHECK(status IN ('complete','partial','rejected')),
 total integer NOT NULL, accepted integer NOT NULL, rejected integer NOT NULL,
 fetched_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source,digest)
);
CREATE TABLE IF NOT EXISTS node_explorer_source_occurrences (
 source text NOT NULL, digest text NOT NULL, position integer NOT NULL,
 identity text NOT NULL REFERENCES node_explorer_node_registry(identity),
 name text NOT NULL,
 PRIMARY KEY(source,digest,position),
 FOREIGN KEY(source,digest) REFERENCES node_explorer_source_batches(source,digest)
);
-- The current flag was write-only (no production code ever read it) and its
-- maintenance rewrote millions of rows per repeat fetch; it is removed.
ALTER TABLE node_explorer_source_occurrences DROP COLUMN IF EXISTS current;
DROP INDEX IF EXISTS node_explorer_source_current;
-- The registry DELETE's foreign-key check looks up occurrences by identity;
-- without this index it seq-scans the whole table per deleted node.
CREATE INDEX IF NOT EXISTS node_explorer_source_occurrences_identity_idx ON node_explorer_source_occurrences(identity);
-- Legacy adoption compares strict configs after removing display-only fields.
-- Hash the JSONB text to keep the index key bounded for large node configs;
-- the query still verifies the complete JSONB value to rule out hash collisions.
CREATE INDEX IF NOT EXISTS node_explorer_registry_raw_config_hash
 ON node_explorer_node_registry (md5(((node->'rawConfig')-'name'-'sub_tag')::text));
-- These three tables were write-only (no production code ever read them)
-- and are removed: source_links (link history), source_rejections
-- (rejection reasons; the counts stay on the batch row), and
-- source_identity_aliases (legacy-adoption audit trail; the adoption itself
-- is recorded in source_connections).
DROP TABLE IF EXISTS node_explorer_source_links;
DROP TABLE IF EXISTS node_explorer_source_rejections;
DROP TABLE IF EXISTS node_explorer_source_identity_aliases;
