import pg from "pg";
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
    close() {
      return pool.end();
    },
  });
}
