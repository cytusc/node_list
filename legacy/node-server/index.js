// ARCHIVED: the Node.js implementation of the collector, scheduler and API.
// The Go service in ../../backend is the runtime; this tree is kept as the
// behavioural reference the Go rewrite was verified against (see
// ../../backend/README.md and the parity tooling in ../../backend/cmd). It is
// no longer built into the container, and nothing in the shipped path imports
// it. The unit tests still run under `npm test` so the reference stays honest.
import path from "node:path";
import { existsSync } from "node:fs";
import { loadEnvFile } from "node:process";
import { fileURLToPath } from "node:url";
import { createPostgresStore } from "./postgres-store.js";
import { createApp } from "./app.js";

const root = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
);
try {
  loadEnvFile(path.join(root, ".env"));
} catch (error) {
  if (error.code !== "ENOENT") throw error;
}
const subDir = path.resolve(
  process.env.SUBS_CHECK_DIR || path.join(root, "data", "sub"),
);
const qilanSessionFile = path.resolve(
  process.env.QILAN_SESSION_FILE ||
    "/mnt/d/code/qilan/qilan-scraper/session.txt",
);
const qilanRefreshSeconds = Number(process.env.QILAN_REFRESH_INTERVAL_SECONDS);
let store;
if (process.env.PGHOST) {
  if (
    !process.env.PGDATABASE ||
    !process.env.PGUSER ||
    !process.env.PGPASSWORD
  ) {
    console.error("PostgreSQL requires PGDATABASE, PGUSER and PGPASSWORD.");
    process.exit(1);
  }
  try {
    store = await createPostgresStore();
  } catch {
    console.error(
      "PostgreSQL initialization failed. Check connection, database and table creation permissions.",
    );
    process.exit(1);
  }
}
const app = createApp({
  store,
  subDir,
  ippure:
    process.env.IPPURE_CONTROLLER && process.env.IPPURE_PROXY_URL
      ? {
          controller: process.env.IPPURE_CONTROLLER,
          proxyUrl: process.env.IPPURE_PROXY_URL,
          secret: process.env.IPPURE_CONTROLLER_SECRET,
          concurrency: Number(process.env.IPPURE_CONCURRENCY || 3),
          apiOrigin: process.env.IPPURE_API_ORIGIN || "https://api.123169.xyz",
        }
      : undefined,
  qilan:
    process.env.QILAN_SESSION || existsSync(qilanSessionFile)
      ? {
          session: process.env.QILAN_SESSION,
          sessionFile: qilanSessionFile,
          userAgent: process.env.QILAN_USER_AGENT || undefined,
          intervalMs: Number.isFinite(qilanRefreshSeconds)
            ? Math.max(60_000, qilanRefreshSeconds * 1000)
            : 15 * 60_000,
          feishuWebhook: process.env.FEISHU_WEBHOOK_URL || undefined,
        }
      : undefined,
  resultsFile: process.env.RESULTS_FILE,
  databasePath: path.resolve(
    process.env.DATABASE_PATH || path.join(root, "data", "nodes.sqlite"),
  ),
  linksFile: path.join(subDir, "output", "base64.txt"),
  clashFile: path.join(subDir, "output", "all.yaml"),
  distDir: path.join(root, "dist"),
});
const port = Number(process.env.API_PORT || 3001);
const server = app.listen(port, process.env.HOST || "127.0.0.1", () =>
  console.log(
    `Node Explorer: http://localhost:${port}\nResults directory: ${subDir}`,
  ),
);

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => {
    server.close(async () => {
      await app.locals.close();
      process.exit(0);
    });
    server.closeIdleConnections();
  });
}
