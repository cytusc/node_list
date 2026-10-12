import { DatabaseSync } from "node:sqlite";
import { mkdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import path from "node:path";
import {
  nodeIdentity,
  aggregateIdentity,
  sourceKey,
  readNodeSources,
  sourceForNode,
  reconcileSnapshot,
} from "./node-sources.js";

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
  "rawLink",
  "rawConfig",
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
    );
    CREATE TABLE IF NOT EXISTS ippure_results (
      snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
      node_index INTEGER NOT NULL, payload TEXT NOT NULL,
      PRIMARY KEY (snapshot_id, node_index)
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
    reconcile(linkedNodes, { prune = false } = {}) {
      db.exec("BEGIN IMMEDIATE");
      try {
        const rows = db
          .prepare("SELECT id, payload FROM snapshots ORDER BY id")
          .all();
        for (const row of rows) {
          const { snapshot, indexes, changed } = reconcileSnapshot(
            JSON.parse(row.payload),
            linkedNodes,
            prune,
          );
          if (!changed) continue;
          const saved = db
            .prepare(
              "SELECT node_index AS nodeIndex, payload FROM ippure_results WHERE snapshot_id = ?",
            )
            .all(row.id);
          db.prepare("DELETE FROM ippure_results WHERE snapshot_id = ?").run(
            row.id,
          );
          // Fingerprints identify the original detection input, not a migration.
          // Keep IDs and empty batch metadata; enrichment cannot collide with an
          // existing fingerprint or create a duplicate on the next import.
          db.prepare(
            "UPDATE snapshots SET node_count = ?, payload = ? WHERE id = ?",
          ).run(snapshot.nodes.length, JSON.stringify(snapshot), row.id);
          for (const item of saved) {
            const nodeIndex = indexes.get(item.nodeIndex);
            if (nodeIndex !== undefined)
              db.prepare("INSERT INTO ippure_results VALUES (?, ?, ?)").run(
                row.id,
                nodeIndex,
                item.payload,
              );
          }
        }
        db.exec("COMMIT");
      } catch (error) {
        db.exec("ROLLBACK");
        throw error;
      }
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
    enrichments(id) {
      return id === undefined
        ? db
            .prepare(
              "SELECT snapshot_id AS snapshotId, node_index AS nodeIndex, payload FROM ippure_results",
            )
            .all()
        : db
            .prepare(
              "SELECT snapshot_id AS snapshotId, node_index AS nodeIndex, payload FROM ippure_results WHERE snapshot_id = ?",
            )
            .all(id);
    },
    saveIPPure(id, index, result) {
      db.prepare(
        "INSERT INTO ippure_results VALUES (?, ?, ?) ON CONFLICT(snapshot_id, node_index) DO UPDATE SET payload = excluded.payload",
      ).run(id, index, JSON.stringify(result));
    },
    close() {
      db.close();
    },
  });
}

export function createSnapshotStore(adapter) {
  let pending;
  let sourceWarning = null;
  let sourcesBackfilled = false;
  async function ingest(file, linksFile) {
    try {
      const raw = JSON.parse(await readFile(file, "utf8"));
      if (
        !Array.isArray(raw.nodes) ||
        raw.nodes.some((n) => !n || typeof n !== "object" || Array.isArray(n))
      ) {
        throw new Error("Invalid snapshot");
      }
      const sources = linksFile ? await readNodeSources(linksFile) : null;
      const snapshot = {
        checkedAt:
          typeof raw.checkedAt === "string" ? raw.checkedAt : undefined,
        speedTest: !!raw.speedTest,
        mediaCheck: !!raw.mediaCheck,
        nodes: raw.nodes.map((n) => {
          const values = Object.fromEntries(
            fields.filter((k) => k in n).map((k) => [k, n[k]]),
          );
          if (sources) {
            const source = sourceForNode(n, sources, { exactNames: true });
            if (!source && !values.rawLink && !values.rawConfig)
              throw new Error(
                "Results and node sources do not match; retry after upstream finishes publishing",
              );
            Object.assign(values, source);
          }
          return values;
        }),
      };
      if (sources && !sourcesBackfilled) {
        const linkedNodes = new Map();
        for (const row of await adapter.all()) {
          for (const node of JSON.parse(row.payload).nodes) {
            const source = sourceForNode(node, sources);
            if (source) linkedNodes.set(sourceKey(node), source);
          }
        }
        for (const node of snapshot.nodes)
          linkedNodes.set(sourceKey(node), {
            ...(node.rawLink ? { rawLink: node.rawLink } : {}),
            ...(node.rawConfig ? { rawConfig: node.rawConfig } : {}),
          });
        await adapter.reconcile(linkedNodes);
        sourcesBackfilled = true;
      }
      // Raw sources are archived alongside a detection batch but are not part
      // of its identity; adding provenance must preserve its IPPure results.
      const original = {
        ...snapshot,
        nodes: snapshot.nodes.map(({ rawLink, rawConfig, ...node }) => node),
      };
      const fingerprint = createHash("sha256")
        .update(JSON.stringify(original))
        .digest("hex");
      const payload = JSON.stringify(snapshot);
      await adapter.insert({ fingerprint, snapshot, payload });
      sourceWarning = null;
    } catch (error) {
      sourceWarning = error.code === "ENOENT" ? "NO_RESULTS" : "READ_FAILED";
      throw error;
    }
  }
  async function hydrate(row, saved) {
    if (!row) return null;
    const snapshot = { ...JSON.parse(row.payload), id: row.id, sourceWarning };
    for (const item of saved ?? (await adapter.enrichments(row.id))) {
      if (snapshot.nodes[item.nodeIndex])
        snapshot.nodes[item.nodeIndex].ippure = JSON.parse(item.payload);
    }
    return snapshot;
  }
  return {
    saveIPPure: (id, index, result) => adapter.saveIPPure(id, index, result),
    reconcileSources: (linkedNodes, options) =>
      adapter.reconcile(linkedNodes, options),
    // Requests and the background collector share a single in-flight import.
    sync(file, linksFile) {
      if (!pending)
        pending = ingest(file, linksFile).finally(() => {
          pending = undefined;
        });
      return pending;
    },
    async get(id) {
      const row = await adapter.get(id);
      return hydrate(row);
    },
    async aggregate() {
      const rows = await adapter.all();
      if (!rows.length) return null;
      const enrichments = new Map();
      for (const item of await adapter.enrichments()) {
        if (!enrichments.has(item.snapshotId))
          enrichments.set(item.snapshotId, []);
        enrichments.get(item.snapshotId).push(item);
      }
      const batches = (
        await Promise.all(
          rows.map(async (row) => ({
            ...(await hydrate(row, enrichments.get(row.id) || [])),
            importedAt: row.importedAt,
          })),
        )
      ).sort((a, b) => {
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
          const key = aggregateIdentity(node);
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
