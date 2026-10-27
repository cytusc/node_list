CREATE TABLE IF NOT EXISTS node_explorer_source_connections (
 version text NOT NULL, fingerprint text NOT NULL, canonical text NOT NULL,
 identity text NOT NULL REFERENCES node_explorer_node_registry(identity),
 first_seen_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(version,fingerprint), UNIQUE(identity)
);
CREATE TABLE IF NOT EXISTS node_explorer_source_batches (
 source text NOT NULL, digest text NOT NULL, snapshot_id integer NOT NULL,
 version text NOT NULL, status text NOT NULL CHECK(status IN ('complete','partial','rejected')),
 total integer NOT NULL, accepted integer NOT NULL, rejected integer NOT NULL,
 fetched_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source,digest)
);
CREATE TABLE IF NOT EXISTS node_explorer_source_occurrences (
 source text NOT NULL, digest text NOT NULL, position integer NOT NULL,
 identity text NOT NULL REFERENCES node_explorer_node_registry(identity),
 name text NOT NULL, current boolean NOT NULL DEFAULT true,
 PRIMARY KEY(source,digest,position),
 FOREIGN KEY(source,digest) REFERENCES node_explorer_source_batches(source,digest)
);
CREATE INDEX IF NOT EXISTS node_explorer_source_current ON node_explorer_source_occurrences(source) WHERE current;
-- Legacy adoption compares strict configs after removing display-only fields.
-- Hash the JSONB text to keep the index key bounded for large node configs;
-- the query still verifies the complete JSONB value to rule out hash collisions.
CREATE INDEX IF NOT EXISTS node_explorer_registry_raw_config_hash
 ON node_explorer_node_registry (md5(((node->'rawConfig')-'name'-'sub_tag')::text));
CREATE TABLE IF NOT EXISTS node_explorer_source_links (
 source text NOT NULL, digest text NOT NULL, position integer NOT NULL,
 link text NOT NULL, origin text NOT NULL, status text NOT NULL, version text NOT NULL,
 PRIMARY KEY(source,digest,position,origin),
 FOREIGN KEY(source,digest) REFERENCES node_explorer_source_batches(source,digest)
);
CREATE TABLE IF NOT EXISTS node_explorer_source_rejections (
 source text NOT NULL, digest text NOT NULL, position integer NOT NULL, code text NOT NULL,
 PRIMARY KEY(source,digest,position),
 FOREIGN KEY(source,digest) REFERENCES node_explorer_source_batches(source,digest)
);
CREATE TABLE IF NOT EXISTS node_explorer_source_identity_aliases (
 legacy_identity text NOT NULL, version text NOT NULL, fingerprint text NOT NULL,
 adopted_identity text NOT NULL, PRIMARY KEY(legacy_identity,version,fingerprint)
);
