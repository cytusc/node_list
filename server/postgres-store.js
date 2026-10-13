import pg from "pg";
import { reconcileSnapshot } from "./node-sources.js";
import { createSnapshotStore } from "./store.js";

export async function createPostgresStore(config) {
  const pool = new pg.Pool({
    max: 3,
    connectionTimeoutMillis: 5000,
    idleTimeoutMillis: 30000,
    query_timeout: 10000,
    statement_timeout: 10000,
    ...config,
  });
  pool.on("error", () =>
    console.error(
      "PostgreSQL connection lost; subsequent requests will retry.",
    ),
  );
  try {
    await pool.query(`CREATE TABLE IF NOT EXISTS node_explorer_snapshots (
      id SERIAL PRIMARY KEY,
      fingerprint TEXT NOT NULL UNIQUE,
      checked_at TEXT,
      imported_at TEXT NOT NULL,
      node_count INTEGER NOT NULL,
      payload TEXT NOT NULL
    );
    CREATE TABLE IF NOT EXISTS node_explorer_ippure_results (
      snapshot_id INTEGER NOT NULL REFERENCES node_explorer_snapshots(id),
      node_index INTEGER NOT NULL, payload TEXT NOT NULL,
      PRIMARY KEY (snapshot_id, node_index)
    );
    CREATE TABLE IF NOT EXISTS node_explorer_retired_nodes (
      identity TEXT PRIMARY KEY,
      retired_at TEXT NOT NULL,
      reason TEXT NOT NULL
    )`);
  } catch (error) {
    await pool.end();
    throw error;
  }
  return createSnapshotStore({
    async insert({ fingerprint, snapshot, payload }) {
      await pool.query(
        `INSERT INTO node_explorer_snapshots
        (fingerprint, checked_at, imported_at, node_count, payload)
        SELECT $1, $2, $3, $4, $5
        WHERE NOT EXISTS (SELECT 1 FROM node_explorer_snapshots WHERE fingerprint = $1)
        ON CONFLICT (fingerprint) DO NOTHING`,
        [
          fingerprint,
          snapshot.checkedAt ?? null,
          new Date().toISOString(),
          snapshot.nodes.length,
          payload,
        ],
      );
    },
    async reconcile(linkedNodes, { prune = false } = {}) {
      const client = await pool.connect();
      try {
        await client.query("BEGIN");
        await client.query(
          "LOCK TABLE node_explorer_snapshots, node_explorer_ippure_results IN EXCLUSIVE MODE",
        );
        const rows = await client.query(
          "SELECT id, payload FROM node_explorer_snapshots ORDER BY id",
        );
        const enrichments = await client.query(
          'SELECT snapshot_id AS "snapshotId", node_index AS "nodeIndex", payload FROM node_explorer_ippure_results',
        );
        const bySnapshot = new Map();
        for (const item of enrichments.rows) {
          if (!bySnapshot.has(item.snapshotId))
            bySnapshot.set(item.snapshotId, []);
          bySnapshot.get(item.snapshotId).push(item);
        }
        for (const row of rows.rows) {
          const { snapshot, indexes, changed } = reconcileSnapshot(
            JSON.parse(row.payload),
            linkedNodes,
            prune,
          );
          const saved = bySnapshot.get(row.id) || [];
          if (!changed) continue;
          await client.query(
            "DELETE FROM node_explorer_ippure_results WHERE snapshot_id = $1",
            [row.id],
          );
          await client.query(
            "UPDATE node_explorer_snapshots SET node_count = $1, payload = $2 WHERE id = $3",
            [snapshot.nodes.length, JSON.stringify(snapshot), row.id],
          );
          for (const item of saved) {
            const nodeIndex = indexes.get(item.nodeIndex);
            if (nodeIndex !== undefined)
              await client.query(
                "INSERT INTO node_explorer_ippure_results VALUES ($1, $2, $3)",
                [row.id, nodeIndex, item.payload],
              );
          }
        }
        await client.query("COMMIT");
      } catch (error) {
        await client.query("ROLLBACK");
        throw error;
      } finally {
        client.release();
      }
    },
    async get(id) {
      const result =
        id === undefined
          ? await pool.query(
              "SELECT id, payload FROM node_explorer_snapshots ORDER BY id DESC LIMIT 1",
            )
          : await pool.query(
              "SELECT id, payload FROM node_explorer_snapshots WHERE id = $1",
              [id],
            );
      return result.rows[0];
    },
    async all() {
      const result = await pool.query(
        'SELECT id, imported_at AS "importedAt", payload FROM node_explorer_snapshots ORDER BY id DESC',
      );
      return result.rows;
    },
    async history(limit, offset) {
      const result = await pool.query(
        `SELECT id, checked_at AS "checkedAt", imported_at AS "importedAt",
        node_count AS "nodeCount" FROM node_explorer_snapshots ORDER BY id DESC LIMIT $1 OFFSET $2`,
        [limit, offset],
      );
      const count = await pool.query(
        "SELECT COUNT(*) AS count FROM node_explorer_snapshots",
      );
      return { items: result.rows, total: Number(count.rows[0].count) };
    },
    async enrichments(id) {
      return (
        await pool.query(
          'SELECT snapshot_id AS "snapshotId", node_index AS "nodeIndex", payload FROM node_explorer_ippure_results' +
            (id === undefined ? "" : " WHERE snapshot_id = $1"),
          id === undefined ? [] : [id],
        )
      ).rows;
    },
    async saveIPPure(id, index, result, identity) {
      await pool.query(
        "INSERT INTO node_explorer_ippure_results SELECT $1::integer, $2::integer, $3::text WHERE NOT EXISTS (SELECT 1 FROM node_explorer_retired_nodes WHERE identity = $4) ON CONFLICT(snapshot_id, node_index) DO UPDATE SET payload = excluded.payload",
        [id, index, JSON.stringify(result), identity],
      );
    },
    async retireNodes(identities, { retiredAt, reason }) {
      const client = await pool.connect();
      try {
        await client.query("BEGIN");
        for (const identity of identities)
          await client.query(
            "INSERT INTO node_explorer_retired_nodes(identity, retired_at, reason) VALUES ($1, $2, $3) ON CONFLICT(identity) DO NOTHING",
            [identity, retiredAt, reason],
          );
        await client.query("COMMIT");
      } catch (error) {
        await client.query("ROLLBACK");
        throw error;
      } finally {
        client.release();
      }
    },
    async retiredNodes() {
      return (
        await pool.query(
          'SELECT identity, retired_at AS "retiredAt", reason FROM node_explorer_retired_nodes',
        )
      ).rows;
    },
    async restoreNode(
      identity,
      retiredAt,
      reason = "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed",
    ) {
      const result = await pool.query(
        "DELETE FROM node_explorer_retired_nodes WHERE identity = $1 AND retired_at = $2 AND reason = $3",
        [identity, retiredAt, reason],
      );
      return result.rowCount;
    },
    close() {
      return pool.end();
    },
  });
}
