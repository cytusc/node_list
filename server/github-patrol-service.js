import path from "node:path";
import { readFile, writeFile, rename, mkdir } from "node:fs/promises";
import { setTimeout as delay } from "node:timers/promises";
import { createStore } from "./store.js";
import { createPostgresStore } from "./postgres-store.js";
import { patrolOnce } from "./github-patrol.js";

const statusFile =
  process.env.GITHUB_PATROL_STATUS_FILE ||
  path.join(
    path.dirname(process.env.DATABASE_PATH || "/data/history/nodes.sqlite"),
    "github-patrol-status.json",
  );
if (process.argv.includes("--health")) {
  try {
    const status = JSON.parse(await readFile(statusFile, "utf8"));
    process.exit(Date.now() - Date.parse(status.updatedAt) < 180000 ? 0 : 1);
  } catch {
    process.exit(1);
  }
}
function number(name, fallback, min, max) {
  const value = Number(process.env[name] || fallback);
  if (!Number.isInteger(value) || value < min || value > max)
    throw new Error(`${name} must be between ${min} and ${max}`);
  return value;
}
const intervalMs =
  number("GITHUB_PATROL_INTERVAL_SECONDS", 1800, 60, 86400) * 1000;
const concurrency = number("GITHUB_PATROL_CONCURRENCY", 2, 1, 5);
const timeoutMs = number("GITHUB_PATROL_TIMEOUT_SECONDS", 12, 3, 60) * 1000;
const retryDelayMs = number("GITHUB_PATROL_RETRY_SECONDS", 10, 1, 300) * 1000;
const controller = process.env.GITHUB_PATROL_CONTROLLER;
const proxyUrl = process.env.GITHUB_PATROL_PROXY_URL;
if (!controller || !proxyUrl)
  throw new Error(
    "GitHub patrol requires a dedicated Mihomo controller and proxy URL",
  );
if (controller === process.env.IPPURE_CONTROLLER)
  throw new Error("GitHub patrol must not share the IPPure controller");
const abort = new AbortController();
for (const event of ["SIGTERM", "SIGINT"])
  process.once(event, () => abort.abort());
const store = process.env.PGHOST
  ? await createPostgresStore()
  : createStore(process.env.DATABASE_PATH || "/data/history/nodes.sqlite");
await mkdir(path.dirname(statusFile), { recursive: true });
const status = {
  enabled: true,
  running: false,
  intervalSeconds: intervalMs / 1000,
  error: null,
};
let writing = Promise.resolve();
function persist() {
  const payload = JSON.stringify({
    ...status,
    updatedAt: new Date().toISOString(),
  });
  writing = writing
    .catch(() => {})
    .then(async () => {
      await writeFile(`${statusFile}.tmp`, payload, { mode: 0o600 });
      await rename(`${statusFile}.tmp`, statusFile);
    });
  return writing;
}
await persist();
const heartbeat = setInterval(
  () =>
    void persist().catch(() => {
      console.error("GitHub patrol status write failed");
    }),
  30000,
);
try {
  while (!abort.signal.aborted) {
    let nextDelayMs = intervalMs;
    Object.assign(status, {
      running: true,
      startedAt: new Date().toISOString(),
      nextRunAt: null,
      progress: null,
      error: null,
    });
    await persist();
    try {
      status.lastResult = await patrolOnce({
        store,
        controller,
        proxyUrl,
        secret: process.env.GITHUB_PATROL_CONTROLLER_SECRET,
        concurrency,
        timeoutMs,
        retryDelayMs,
        signal: abort.signal,
        onProgress: async (progress) => {
          status.progress = progress;
          await persist();
        },
      });
      status.completedAt = new Date().toISOString();
      console.log(
        JSON.stringify({
          event: "github-patrol-complete",
          ...status.lastResult,
        }),
      );
    } catch {
      if (abort.signal.aborted) break;
      status.error =
        "巡检未完成：数据库、代理配置或网络异常，未确认的节点不做软删除";
      nextDelayMs = Math.min(intervalMs, 60000);
      console.error(status.error);
    }
    status.running = false;
    status.nextRunAt = new Date(Date.now() + nextDelayMs).toISOString();
    await persist();
    await delay(nextDelayMs, undefined, { signal: abort.signal });
  }
} catch (error) {
  if (!abort.signal.aborted) throw error;
} finally {
  clearInterval(heartbeat);
  status.running = false;
  await persist();
  await store.close();
}
