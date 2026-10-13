import { aggregateIdentity } from "./node-sources.js";
import { createSnapshotStore } from "./store.js";
import {
  compareResultValues,
  resultSearchValues,
  resultSortKeys,
  resultSortValue,
  resultsMetadata,
  resultsSort,
} from "./results-query.js";

const sortColumns = Object.fromEntries(
  resultSortKeys.map((key) => [key, `rank_${key.toLowerCase()}`]),
);
const columns = {
  position: "integer",
  identity: "text",
  payload: "text",
  country: "text",
  type: "text",
  network: "text",
  tls: "boolean",
  reality: "boolean",
  udp: "boolean",
  speed: "double precision",
  search_values: "text[]",
  media_available: "text[]",
  ...Object.fromEntries(
    Object.values(sortColumns).map((name) => [name, "integer"]),
  ),
};
const lockKey =
  "hashtext(current_schema()), hashtext('node-explorer-results-v1')";

// Projection data is disposable; snapshots, source configurations and enrichment
// remain authoritative. Row triggers also detect changes made by other services.
export async function initializeResultsProjection(pool) {
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await client.query(`SELECT pg_advisory_xact_lock(${lockKey})`);
    await client.query(`
      CREATE TABLE IF NOT EXISTS node_explorer_results_state (
        singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
        source_revision bigint NOT NULL DEFAULT 0,
        built_revision bigint NOT NULL DEFAULT -1,
        retired_revision bigint NOT NULL DEFAULT 0,
        metadata jsonb
      );
      CREATE TABLE IF NOT EXISTS node_explorer_results_changes (
        snapshot_id integer PRIMARY KEY,
        revision bigint NOT NULL
      );
      INSERT INTO node_explorer_results_state(singleton) VALUES (true)
        ON CONFLICT DO NOTHING;
      CREATE TABLE IF NOT EXISTS node_explorer_current_nodes (
        ${Object.entries(columns)
          .map(([name, type]) => `${name} ${type}`)
          .join(",")},
        PRIMARY KEY (position), UNIQUE (identity) DEFERRABLE INITIALLY DEFERRED
      );
      CREATE INDEX IF NOT EXISTS node_explorer_current_speed
        ON node_explorer_current_nodes(rank_speed DESC NULLS LAST, position);
      CREATE INDEX IF NOT EXISTS node_explorer_current_name
        ON node_explorer_current_nodes(rank_name, position);
      CREATE INDEX IF NOT EXISTS node_explorer_current_country_speed
        ON node_explorer_current_nodes(country, rank_speed DESC NULLS LAST, position);
      CREATE INDEX IF NOT EXISTS node_explorer_current_type_speed
        ON node_explorer_current_nodes(type, rank_speed DESC NULLS LAST, position);
      CREATE OR REPLACE FUNCTION node_explorer_results_changed() RETURNS trigger
        LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
        DECLARE revision bigint; snapshot integer;
        BEGIN
          UPDATE node_explorer_results_state SET source_revision = source_revision + 1
            RETURNING source_revision INTO revision;
          IF TG_TABLE_NAME = 'node_explorer_retired_nodes' THEN
            UPDATE node_explorer_results_state SET retired_revision = revision;
          ELSE
            IF TG_TABLE_NAME = 'node_explorer_snapshots' THEN
              snapshot := CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
            ELSE
              snapshot := CASE WHEN TG_OP = 'DELETE' THEN OLD.snapshot_id ELSE NEW.snapshot_id END;
            END IF;
            INSERT INTO node_explorer_results_changes(snapshot_id, revision) VALUES (snapshot, revision)
              ON CONFLICT (snapshot_id) DO UPDATE SET revision = EXCLUDED.revision;
          END IF;
          RETURN NULL;
        END $$;
    `);
    for (const table of ["snapshots", "ippure_results", "retired_nodes"]) {
      const name = `node_explorer_${table}`;
      await client.query(`DO $$ BEGIN
        IF NOT EXISTS (SELECT 1 FROM pg_trigger
          WHERE tgrelid = '${name}'::regclass AND tgname = 'results_changed') THEN
          CREATE TRIGGER results_changed AFTER INSERT OR UPDATE OR DELETE ON ${name}
            FOR EACH ROW EXECUTE FUNCTION node_explorer_results_changed();
        END IF;
      END $$`);
    }
    await client.query("COMMIT");
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

function projectionRows(snapshot) {
  const rows = snapshot.nodes.map((node, position) => ({
    position,
    identity: aggregateIdentity(node),
    payload: JSON.stringify(node),
    country: node.country || "unknown",
    type: node.type || "unknown",
    network: node.network || "unknown",
    tls: !!node.tls,
    reality: !!node.reality,
    udp: !!node.udp,
    speed: node.speed ?? 0,
    search_values: resultSearchValues(node),
    media_available: [
      ...new Set(
        (node.media || [])
          .filter((m) => m.tag || m.Tag)
          .map((m) => m.platform || m.Platform || "")
          .filter(Boolean),
      ),
    ],
  }));
  // Persist Intl's Chinese/numeric ordering so PostgreSQL collation differences
  // cannot change page boundaries. Equal values retain the aggregate's order.
  for (const key of resultSortKeys) {
    const ordered = snapshot.nodes
      .map((node, position) => ({
        position,
        value: resultSortValue(node, key),
      }))
      .filter(({ value }) => value != null)
      .sort((a, b) => compareResultValues(a.value, b.value));
    for (const row of rows) row[sortColumns[key]] = null;
    let rank = 0,
      previous;
    for (const item of ordered) {
      if (previous && compareResultValues(previous.value, item.value) !== 0)
        rank++;
      rows[item.position][sortColumns[key]] = rank;
      previous = item;
    }
  }
  return rows;
}

// This runs in a worker thread. Read one consistent source generation, then
// atomically publish it. Concurrent changes stay dirty for the next refresh.
export async function refreshResultsProjection(pool, cache = {}) {
  const started = Date.now();
  const client = await pool.connect();
  let locked = false;
  try {
    locked = (
      await client.query(`SELECT pg_try_advisory_lock(${lockKey}) AS locked`)
    ).rows[0].locked;
    if (!locked) return { updated: false };
    await client.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY");
    const state = (
      await client.query(
        "SELECT source_revision, built_revision, retired_revision FROM node_explorer_results_state",
      )
    ).rows[0];
    if (state.source_revision === state.built_revision) {
      // Another process may have published newer data while this worker was
      // idle. Never use its old decoded batches for collection candidates.
      if (cache.rows && cache.revision !== state.built_revision) {
        for (const key of Object.keys(cache)) delete cache[key];
      }
      await client.query("COMMIT");
      return { updated: false };
    }
    // A persistent worker retains decoded batches and connection identities.
    // Only changed batches cross the database connection after the first build.
    // If another worker published and pruned our change log, start from source.
    const incremental =
      cache.rows && BigInt(cache.revision) >= BigInt(state.built_revision);
    const changedIds = incremental
      ? (
          await client.query(
            "SELECT snapshot_id FROM node_explorer_results_changes WHERE revision > $1",
            [cache.revision],
          )
        ).rows.map((row) => row.snapshot_id)
      : undefined;
    const rows = (
      await client.query(
        'SELECT id, imported_at AS "importedAt", payload FROM node_explorer_snapshots' +
          (incremental ? " WHERE id = ANY($1::integer[])" : "") +
          " ORDER BY id DESC",
        incremental ? [changedIds] : [],
      )
    ).rows;
    const enrichments = (
      await client.query(
        'SELECT snapshot_id AS "snapshotId", node_index AS "nodeIndex", payload FROM node_explorer_ippure_results' +
          (incremental ? " WHERE snapshot_id = ANY($1::integer[])" : ""),
        incremental ? [changedIds] : [],
      )
    ).rows;
    const retired =
      incremental && BigInt(state.retired_revision) <= BigInt(cache.revision)
        ? cache.retired
        : (
            await client.query(
              'SELECT identity, retired_at AS "retiredAt", reason FROM node_explorer_retired_nodes',
            )
          ).rows;
    await client.query("COMMIT");
    const allRows = incremental ? new Map(cache.rows) : new Map();
    const allEnrichments = incremental ? new Map(cache.enrichments) : new Map();
    for (const id of changedIds || []) {
      allRows.delete(id);
      allEnrichments.delete(id);
    }
    for (const row of rows) {
      row.snapshot = JSON.parse(row.payload);
      row.nodeIdentities = row.snapshot.nodes.map(aggregateIdentity);
      delete row.payload;
      allRows.set(row.id, row);
    }
    for (const item of enrichments) {
      if (!allEnrichments.has(item.snapshotId))
        allEnrichments.set(item.snapshotId, []);
      allEnrichments.get(item.snapshotId).push(item);
    }
    const store = createSnapshotStore({
      all: async () => [...allRows.values()].sort((a, b) => b.id - a.id),
      enrichments: async () => [...allEnrichments.values()].flat(),
      retiredNodes: async () => retired,
    });
    const snapshot = await store.aggregate();
    const nodes = snapshot ? projectionRows(snapshot) : [];
    const metadata = snapshot ? resultsMetadata(snapshot) : null;
    const projection = new Map(
      nodes.map((row) => [row.position, JSON.stringify(row)]),
    );
    const canPatch =
      cache.projection && cache.revision === state.built_revision;
    const changedNodes = canPatch
      ? nodes.filter(
          (row) =>
            projection.get(row.position) !== cache.projection.get(row.position),
        )
      : nodes;
    await client.query("BEGIN");
    if (canPatch) {
      const removed = [...cache.projection.keys()].filter(
        (position) => !projection.has(position),
      );
      if (removed.length)
        await client.query(
          "DELETE FROM node_explorer_current_nodes WHERE position = ANY($1::integer[])",
          [removed],
        );
    } else {
      await client.query("DELETE FROM node_explorer_current_nodes");
    }
    const definitions = Object.entries(columns)
      .map(([name, type]) => `${name} ${type}`)
      .join(",");
    const assignments = Object.keys(columns)
      .filter((name) => name !== "position")
      .map((name) => `${name} = EXCLUDED.${name}`)
      .join(",");
    for (let offset = 0; offset < changedNodes.length; offset += 250) {
      await client.query(
        `INSERT INTO node_explorer_current_nodes
        SELECT * FROM jsonb_to_recordset($1::jsonb) AS n(${definitions})
        ON CONFLICT (position) DO UPDATE SET ${assignments}`,
        [JSON.stringify(changedNodes.slice(offset, offset + 250))],
      );
    }
    await client.query(
      "UPDATE node_explorer_results_state SET built_revision = $1, metadata = $2::jsonb",
      [state.source_revision, JSON.stringify(metadata)],
    );
    await client.query(
      "DELETE FROM node_explorer_results_changes WHERE revision <= $1",
      [state.source_revision],
    );
    await client.query("COMMIT");
    Object.assign(cache, {
      rows: allRows,
      enrichments: allEnrichments,
      retired,
      revision: state.source_revision,
      projection,
    });
    await client.query("ANALYZE node_explorer_current_nodes");
    return {
      updated: true,
      nodes: nodes.length,
      batchesRead: rows.length,
      nodesWritten: changedNodes.length,
      incremental: !!incremental,
      ms: Date.now() - started,
    };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    try {
      if (locked) await client.query(`SELECT pg_advisory_unlock(${lockKey})`);
    } finally {
      client.release();
    }
  }
}

export function cachedSnapshotStore(cache) {
  return createSnapshotStore({
    all: async () => [...cache.rows.values()].sort((a, b) => b.id - a.id),
    enrichments: async () => [...cache.enrichments.values()].flat(),
    retiredNodes: async () => cache.retired,
  });
}

export async function queryResultsProjection(pool, query) {
  const client = await pool.connect();
  try {
    await client.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY");
    const metadata = (
      await client.query("SELECT metadata FROM node_explorer_results_state")
    ).rows[0]?.metadata;
    if (!metadata) {
      await client.query("COMMIT");
      return null;
    }
    if (!query) {
      const nodes = (
        await client.query(
          "SELECT payload FROM node_explorer_current_nodes ORDER BY position",
        )
      ).rows.map((row) => JSON.parse(row.payload));
      await client.query("COMMIT");
      const {
        totalNodes: _total,
        summary: _summary,
        facets: _facets,
        ...snapshot
      } = metadata;
      return { ...snapshot, nodes };
    }
    const values = [],
      conditions = [];
    const param = (value) => {
      values.push(value);
      return `$${values.length}`;
    };
    for (const [column, list] of [
      ["country", query.countries],
      ["type", query.types],
      ["network", query.networks],
    ]) {
      if (list.length)
        conditions.push(`${column} = ANY(${param(list)}::text[])`);
    }
    // These names have already been validated, but keep an explicit whitelist
    // at the SQL boundary as well. All user values use bound parameters.
    for (const feature of ["tls", "reality", "udp"]) {
      if (query.features.includes(feature)) conditions.push(feature);
    }
    if (query.q)
      conditions.push(
        `EXISTS (SELECT 1 FROM unnest(search_values) AS v WHERE strpos(v, ${param(query.q)}) > 0)`,
      );
    if (metadata.speedTest && query.minSpeed > 0)
      conditions.push(`speed >= ${param(query.minSpeed * 1024)}`);
    if (metadata.mediaCheck && query.media.length)
      conditions.push(`${param(query.media)}::text[] <@ media_available`);
    const where = conditions.length ? `WHERE ${conditions.join(" AND ")}` : "";
    const total = Number(
      (
        await client.query(
          `SELECT count(*) AS total FROM node_explorer_current_nodes ${where}`,
          values,
        )
      ).rows[0].total,
    );
    const page = Math.min(
      query.page,
      Math.max(1, Math.ceil(total / query.limit)),
    );
    const { key, direction } = resultsSort(metadata, query);
    const order = direction === "asc" ? "ASC" : "DESC";
    const limit = param(query.limit),
      offset = param((page - 1) * query.limit);
    const nodes = (
      await client.query(
        `SELECT payload FROM node_explorer_current_nodes ${where}
      ORDER BY ${sortColumns[key]} ${order} NULLS LAST, position
      LIMIT ${limit} OFFSET ${offset}`,
        values,
      )
    ).rows.map((row) => JSON.parse(row.payload));
    await client.query("COMMIT");
    return { ...metadata, nodes, total, page, limit: query.limit };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}
