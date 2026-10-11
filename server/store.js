import { DatabaseSync } from "node:sqlite";
import { mkdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import path from "node:path";

const fields = [
  "name",
  "baseName",
  "type",
  "server",
  "port",
  "sni",
  "tls",
  "reality",
  "udp",
  "network",
  "speed",
  "country",
  "ip",
  "ipRisk",
  "media",
  "subTag",
  "details",
];

export function createStore(databasePath = ":memory:") {
  if (databasePath !== ":memory:")
    mkdirSync(path.dirname(databasePath), { recursive: true });
  const db = new DatabaseSync(databasePath);
  db.exec(`PRAGMA journal_mode = WAL;
    PRAGMA busy_timeout = 5000;
    CREATE TABLE IF NOT EXISTS snapshots (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      fingerprint TEXT NOT NULL UNIQUE,
      checked_at TEXT,
      imported_at TEXT NOT NULL,
      node_count INTEGER NOT NULL,
      payload TEXT NOT NULL
    );`);
  return createSnapshotStore({
    insert({ fingerprint, snapshot, payload }) {
      if (
        !db
          .prepare("SELECT id FROM snapshots WHERE fingerprint = ?")
          .get(fingerprint)
      )
        db.prepare(
          `INSERT OR IGNORE INTO snapshots
          (fingerprint, checked_at, imported_at, node_count, payload) VALUES (?, ?, ?, ?, ?)`,
        ).run(
          fingerprint,
          snapshot.checkedAt ?? null,
          new Date().toISOString(),
          snapshot.nodes.length,
          payload,
        );
    },
    get(id) {
      return id === undefined
        ? db
            .prepare(
              "SELECT id, payload FROM snapshots ORDER BY id DESC LIMIT 1",
            )
            .get()
        : db.prepare("SELECT id, payload FROM snapshots WHERE id = ?").get(id);
    },
    all() {
      return db
        .prepare(
          "SELECT id, imported_at AS importedAt, payload FROM snapshots ORDER BY id DESC",
        )
        .all();
    },
    history(limit, offset) {
      return {
        items: db
          .prepare(
            `SELECT id, checked_at AS checkedAt, imported_at AS importedAt,
          node_count AS nodeCount FROM snapshots ORDER BY id DESC LIMIT ? OFFSET ?`,
          )
          .all(limit, offset),
        total: db.prepare("SELECT COUNT(*) AS count FROM snapshots").get()
          .count,
      };
    },
    close() {
      db.close();
    },
  });
}

export function createSnapshotStore(adapter) {
  let pending;
  let sourceWarning = null;
  async function ingest(file) {
    try {
      const raw = JSON.parse(await readFile(file, "utf8"));
      if (
        !Array.isArray(raw.nodes) ||
        raw.nodes.some((n) => !n || typeof n !== "object" || Array.isArray(n))
      ) {
        throw new Error("Invalid snapshot");
      }
      const snapshot = {
        checkedAt:
          typeof raw.checkedAt === "string" ? raw.checkedAt : undefined,
        speedTest: !!raw.speedTest,
        mediaCheck: !!raw.mediaCheck,
        nodes: raw.nodes.map((n) =>
          Object.fromEntries(
            fields.filter((k) => k in n).map((k) => [k, n[k]]),
          ),
        ),
      };
      const payload = JSON.stringify(snapshot);
      const fingerprint = createHash("sha256").update(payload).digest("hex");
      await adapter.insert({ fingerprint, snapshot, payload });
      sourceWarning = null;
    } catch (error) {
      sourceWarning = error.code === "ENOENT" ? "NO_RESULTS" : "READ_FAILED";
      throw error;
    }
  }
  return {
    // Requests and the background collector share a single in-flight import.
    sync(file) {
      if (!pending)
        pending = ingest(file).finally(() => {
          pending = undefined;
        });
      return pending;
    },
    async get(id) {
      const row = await adapter.get(id);
      return row
        ? { ...JSON.parse(row.payload), id: row.id, sourceWarning }
        : null;
    },
    async aggregate() {
      const rows = await adapter.all();
      if (!rows.length) return null;
      const batches = rows
        .map((row) => ({
          ...JSON.parse(row.payload),
          id: row.id,
          importedAt: row.importedAt,
        }))
        .sort((a, b) => {
          const time = (batch) => {
            const checked = Date.parse(batch.checkedAt);
            return Number.isFinite(checked)
              ? checked
              : Date.parse(batch.importedAt) || 0;
          };
          return time(b) - time(a) || b.id - a.id;
        });
      const nodes = new Map();
      for (const batch of batches) {
        for (const node of batch.nodes) {
          // Names, exit IPs and test results can change between checks.
          const key = nodeIdentity(node);
          if (!nodes.has(key))
            nodes.set(key, {
              ...node,
              checkedAt: batch.checkedAt,
              snapshotId: batch.id,
              speedTest: batch.speedTest,
              mediaCheck: batch.mediaCheck,
            });
        }
      }
      const result = [...nodes.values()];
      return {
        checkedAt: batches[0].checkedAt,
        batchCount: batches.length,
        speedTest: result.length
          ? result.some((node) => node.speedTest)
          : batches[0].speedTest,
        mediaCheck: result.length
          ? result.some((node) => node.mediaCheck)
          : batches[0].mediaCheck,
        nodes: result,
        sourceWarning,
      };
    },
    history: (limit, offset) => adapter.history(limit, offset),
    close: () => adapter.close(),
  };
}

function nodeIdentity(node) {
  const details = (node.details || [])
    .map(({ k, v }) => [k, v])
    .sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
  return JSON.stringify([
    node.type?.toLowerCase(),
    node.server?.toLowerCase(),
    String(node.port ?? ""),
    node.sni?.toLowerCase() || "",
    node.network?.toLowerCase() || "",
    !!node.tls,
    !!node.reality,
    !!node.udp,
    details,
    // Incomplete legacy records cannot be identified by an endpoint alone.
    node.server ? "" : node.baseName || node.name,
  ]);
}
