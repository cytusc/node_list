import { DatabaseSync } from "node:sqlite";
import { mkdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import path from "node:path";
import { EXIT_IP_VERSION, hasVerifiedExitIP } from "./exit-ip.js";
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

// Only retirements with this reason are restorable by the scheduled patrol
// recheck; manual or other automated retirements stay until handled explicitly.
const RETIRE_RESTORE_REASON =
  "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed";

// Classify nodes by the verified exit IP's country: source-claimed regions
// frequently lie. The original claim is kept alongside for the mismatch badge.
export function applyExitCountry(node) {
  const country = node.ippure?.country;
  if (typeof country !== "string" || !/^[A-Za-z]{2}$/.test(country)) return;
  const verified = country.toUpperCase();
  if (node.country && node.country.toUpperCase() !== verified) {
    node.claimedCountry = node.country;
    node.country = verified;
  } else if (!node.country) {
    node.country = verified;
  }
}

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
    );
    CREATE TABLE IF NOT EXISTS retired_nodes (
      identity TEXT PRIMARY KEY,
      retired_at TEXT NOT NULL,
      reason TEXT NOT NULL
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
    saveIPPure(id, index, result, identity) {
      db.prepare(
        "INSERT INTO ippure_results SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM retired_nodes WHERE identity = ?) ON CONFLICT(snapshot_id, node_index) DO UPDATE SET payload = excluded.payload",
      ).run(id, index, JSON.stringify(result), identity);
    },
    retireNodes(identities, { retiredAt, reason }) {
      const save = db.prepare(
        "INSERT INTO retired_nodes(identity, retired_at, reason) VALUES (?, ?, ?) ON CONFLICT(identity) DO NOTHING",
      );
      db.exec("BEGIN IMMEDIATE");
      try {
        for (const identity of identities)
          save.run(identity, retiredAt, reason);
        db.exec("COMMIT");
      } catch (error) {
        db.exec("ROLLBACK");
        throw error;
      }
    },
    retiredNodes() {
      return db
        .prepare(
          "SELECT identity, retired_at AS retiredAt, reason FROM retired_nodes",
        )
        .all();
    },
    restoreNode(identity, retiredAt, reason = RETIRE_RESTORE_REASON) {
      return db
        .prepare(
          "DELETE FROM retired_nodes WHERE identity = ? AND retired_at = ? AND reason = ?",
        )
        .run(identity, retiredAt, reason).changes;
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
  async function ingestRaw(raw, linksFile, updateSourceWarning = true) {
    try {
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
      if (updateSourceWarning) sourceWarning = null;
    } catch (error) {
      if (updateSourceWarning)
        sourceWarning = error.code === "ENOENT" ? "NO_RESULTS" : "READ_FAILED";
      throw error;
    }
  }
  async function ingest(file, linksFile) {
    try {
      return await ingestRaw(JSON.parse(await readFile(file, "utf8")), linksFile);
    } catch (error) {
      sourceWarning = error.code === "ENOENT" ? "NO_RESULTS" : "READ_FAILED";
      throw error;
    }
  }
  async function hydrate(row, saved, retiredRows) {
    if (!row) return null;
    const snapshot = { ...JSON.parse(row.payload), id: row.id, sourceWarning };
    for (const item of saved ?? (await adapter.enrichments(row.id))) {
      if (snapshot.nodes[item.nodeIndex]) {
        snapshot.nodes[item.nodeIndex].ippure = JSON.parse(item.payload);
        applyExitCountry(snapshot.nodes[item.nodeIndex]);
      }
    }
    // Before collection preserved original indexes, a filtered current batch
    // could attach later nodes' results to earlier rows. Retain the database
    // records, but rediscover ambiguous exits written after that retirement.
    const retirements = new Map(
      (retiredRows ?? (await adapter.retiredNodes())).map((item) => [
        item.identity,
        Date.parse(item.retiredAt),
      ]),
    );
    let earliestRetirement = Infinity;
    for (const node of snapshot.nodes) {
      const retiredAt = retirements.get(aggregateIdentity(node));
      if (Number.isFinite(retiredAt))
        earliestRetirement = Math.min(earliestRetirement, retiredAt);
      if (
        node.ippure?.exitIPVersion !== EXIT_IP_VERSION &&
        Date.parse(node.ippure?.checkedAt) >= earliestRetirement
      )
        delete node.ippure;
    }
    return snapshot;
  }
  async function visible(
    snapshot,
    { includeRetired = false, preserveIndexes = false } = {},
  ) {
    if (!snapshot || includeRetired) return snapshot;
    const retired = new Set(
      (await adapter.retiredNodes()).map((item) => item.identity),
    );
    const entries = snapshot.nodes
      .map((node, index) => ({ node, index }))
      .filter(({ node }) => !retired.has(aggregateIdentity(node)));
    return {
      ...snapshot,
      nodes: entries.map(({ node }) => node),
      ...(preserveIndexes
        ? { nodeIndexes: entries.map(({ index }) => index) }
        : {}),
    };
  }
  return {
    async saveIPPure(id, index, result) {
      const row = await adapter.get(id);
      const node = row && JSON.parse(row.payload).nodes[index];
      if (!node) return;
      await adapter.saveIPPure(id, index, result, aggregateIdentity(node));
    },
    async retireNodes(nodes, reason = "GitHub probe failed twice") {
      const identities = [...new Set(nodes.map(aggregateIdentity))];
      if (identities.some((identity) => !identity))
        throw new Error("Cannot retire a node without a stable identity");
      await adapter.retireNodes(identities, {
        retiredAt: new Date().toISOString(),
        reason,
      });
      return identities.length;
    },
    async retiredNodeIdentities() {
      return new Set(
        (await adapter.retiredNodes()).map((item) => item.identity),
      );
    },
    retiredNodes: () => adapter.retiredNodes(),
    restoreNode: (identity, retiredAt, reason) =>
      adapter.restoreNode(identity, retiredAt, reason),
    reconcileSources: (linkedNodes, options) =>
      adapter.reconcile(linkedNodes, options),
    // Requests and the background collector share a single in-flight import.
    sync(file, linksFile) {
      if (!pending) {
        let current;
        current = ingest(file, linksFile).finally(() => {
          if (pending === current) pending = undefined;
        });
        pending = current;
      }
      return pending;
    },
    syncSnapshot(snapshot) {
      const next = (pending || Promise.resolve())
        .catch(() => {})
        .then(() => ingestRaw(snapshot, undefined, false));
      let current;
      current = next.finally(() => {
        if (pending === current) pending = undefined;
      });
      pending = current;
      return current;
    },
    async get(id) {
      const row = await adapter.get(id);
      return visible(await hydrate(row), { includeRetired: id !== undefined });
    },
    async getCollectionSnapshot() {
      return visible(await hydrate(await adapter.get()), {
        preserveIndexes: true,
      });
    },
    async aggregate({ includeRetired = false } = {}) {
      const rows = await adapter.all();
      if (!rows.length) return null;
      const enrichments = new Map();
      for (const item of await adapter.enrichments()) {
        if (!enrichments.has(item.snapshotId))
          enrichments.set(item.snapshotId, []);
        enrichments.get(item.snapshotId).push(item);
      }
      const retiredRows = await adapter.retiredNodes();
      const retired = new Set(retiredRows.map((item) => item.identity));
      const batches = (
        await Promise.all(
          rows.map(async (row) => ({
            ...(await hydrate(row, enrichments.get(row.id) || [], retiredRows)),
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
          if (!includeRetired && retired.has(key)) continue;
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
      // A source refresh archives a new row without changing the connection.
      // Keep its most recent IP enrichment visible across detection batches.
      const latestEnrichments = new Map();
      for (const batch of batches)
        for (const node of batch.nodes) {
          if (!node.ippure || node.rawConfig || !node.rawLink) continue;
          const key = aggregateIdentity(node),
            previous = latestEnrichments.get(key);
          if (
            !previous ||
            (Date.parse(node.ippure.checkedAt) || 0) >
              (Date.parse(previous.checkedAt) || 0)
          )
            latestEnrichments.set(key, node.ippure);
        }
      for (const node of result) {
        if (node.rawConfig || !node.rawLink) continue;
        const saved = latestEnrichments.get(aggregateIdentity(node));
        if (saved) node.ippure = saved;
      }
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
    async ippureCandidates() {
      const rows = await adapter.all();
      const enrichments = new Map();
      for (const item of await adapter.enrichments()) {
        if (!enrichments.has(item.snapshotId))
          enrichments.set(item.snapshotId, []);
        enrichments.get(item.snapshotId).push(item);
      }
      const candidates = [];
      const retiredRows = await adapter.retiredNodes();
      const retired = new Set(retiredRows.map((item) => item.identity));
      for (const row of rows) {
        const snapshot = await hydrate(
          row,
          enrichments.get(row.id) || [],
          retiredRows,
        );
        snapshot.nodes.forEach((node, index) => {
          if (
            !retired.has(aggregateIdentity(node)) &&
            (node.ippure?.status !== "success" ||
              !hasVerifiedExitIP(node.ippure) ||
              !node.ippure.asnOrganization) &&
            (node.rawConfig || node.rawLink)
          )
            candidates.push({ snapshotId: row.id, index, node });
        });
      }
      return candidates;
    },
    history: (limit, offset) => adapter.history(limit, offset),
    close: () => adapter.close(),
  };
}
