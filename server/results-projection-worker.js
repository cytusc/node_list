import { parentPort, workerData } from "node:worker_threads";
import pg from "pg";
import {
  cachedSnapshotStore,
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
parentPort.on("message", ({ id, operation, args }) => {
  pending = pending.then(async () => {
    try {
      let result;
      if (operation === "sync")
        result = await store.sync(args.file, args.sources);
      else if (operation === "refresh")
        result = await refreshResultsProjection(pool, projectionCache);
      else if (operation === "aggregate") {
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
