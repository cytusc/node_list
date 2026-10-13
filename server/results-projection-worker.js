import { parentPort, workerData } from "node:worker_threads";
import pg from "pg";
import {
  cachedSnapshotStore,
  pruneOldSnapshots,
  refreshResultsProjection,
  queryResultsProjection,
} from "./postgres-results.js";
import { createPostgresStore } from "./postgres-store.js";

const pool = new pg.Pool({ ...workerData.config, max: 1 });
const store = await createPostgresStore(workerData.config, {
  maintenanceWorker: true,
});
let pending = Promise.resolve();
const projectionCache = {};
// Retention is PG-only (SQLite stores prune manually); 0 disables pruning.
const retentionDays = Number(process.env.SNAPSHOT_RETENTION_DAYS ?? 7);
const prune = async () => {
  const { pruned } = await pruneOldSnapshots(pool, projectionCache, retentionDays);
  // Dropped batches must not serve stale rows from the in-memory cache; the
  // revision bump makes the next refresh rebuild from source anyway.
  if (pruned) for (const key of Object.keys(projectionCache)) delete projectionCache[key];
};
parentPort.on("message", ({ id, operation, args }) => {
  pending = pending.then(async () => {
    try {
      let result;
      if (operation === "sync")
        result = await store.sync(args.file, args.sources);
      else if (operation === "refresh") {
        result = await refreshResultsProjection(pool, projectionCache);
        await prune();
      } else if (operation === "aggregate") {
        if (args?.includeRetired) result = await store.aggregate(args);
        else {
          await refreshResultsProjection(pool, projectionCache);
          result = await queryResultsProjection(pool);
        }
      } else if (operation === "candidates") {
        await refreshResultsProjection(pool, projectionCache);
        result = projectionCache.rows
          ? await cachedSnapshotStore(projectionCache).ippureCandidates()
          : await store.ippureCandidates();
      } else if (operation === "collect-context") {
        // One refresh per collection round feeds all three reads.
        await refreshResultsProjection(pool, projectionCache);
        await prune();
        result = {
          snapshot: await store.getCollectionSnapshot(),
          historical: projectionCache.rows
            ? await cachedSnapshotStore(projectionCache).ippureCandidates()
            : await store.ippureCandidates(),
          aggregate: await queryResultsProjection(pool),
        };
      } else if (operation === "close") {
        await store.close();
        await pool.end();
      } else throw new Error("Unknown maintenance operation");
      parentPort.postMessage({ id, result });
      if (operation === "close") parentPort.close();
    } catch (error) {
      parentPort.postMessage({
        id,
        error: "Database background maintenance failed.",
        code: error.code,
      });
    }
  });
});
