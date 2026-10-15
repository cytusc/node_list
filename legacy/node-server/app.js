import express from "express";
import { parseResultsQuery, paginateResults } from "./results-query.js";
import path from "node:path";
import { createStore } from "./store.js";
import { startIPPureCollector } from "./ippure.js";
import { startQilanSource } from "./qilan-source.js";

function publicSnapshot(snapshot) {
  if (!snapshot) return snapshot;
  return {
    ...snapshot,
    nodes: snapshot.nodes.map(
      ({ rawLink: _rawLink, rawConfig: _rawConfig, ...node }) => node,
    ),
  };
}

export function createApp({
  subDir,
  resultsFile,
  distDir,
  databasePath,
  linksFile,
  clashFile,
  store: configuredStore,
  importIntervalMs = 5000,
  ippure,
  qilan,
} = {}) {
  const app = express();
  app.disable("x-powered-by");
  app.get("/api/health", (_req, res) => res.json({ status: "ok" }));
  const snapshotPath =
    resultsFile ||
    path.join(subDir, "config", "cache", "config", "results.json");
  const store = configuredStore || createStore(databasePath);
  const sources = clashFile ? { clashFile, linksFile } : linksFile;
  let lastSourceError;
  let backgroundSync;
  const sync = () => {
    if (!backgroundSync) {
      backgroundSync = (async () => {
        try {
          await store.sync(snapshotPath, sources);
          lastSourceError = undefined;
        } catch (error) {
          lastSourceError = error;
        }
        // IPPure and patrol changes in other processes invalidate the shared
        // database projection. Heavy refresh work runs outside the HTTP thread.
        await store.refreshAggregate?.();
      })()
        .catch((error) => {
          console.error(
            "Background result refresh failed; subsequent refreshes will retry.",
            error.code || "REFRESH_FAILED",
          );
        })
        .finally(() => {
          backgroundSync = undefined;
        });
    }
    return backgroundSync;
  };
  const initialSync = sync();
  const timer = setInterval(sync, importIntervalMs);
  timer.unref();
  let collector = null;
  let qilanSource = null;
  const collectorReady = initialSync.then(() => {
    if (ippure) collector = startIPPureCollector({ store, subDir, ...ippure });
    if (qilan) qilanSource = startQilanSource({ store, ...qilan });
  });
  app.locals.close = async () => {
    clearInterval(timer);
    await collectorReady;
    await collector?.close();
    await qilanSource?.close();
    await sync();
    await store.close();
  };
  app.get("/api/history", async (req, res) => {
    res.set("Cache-Control", "no-store");
    const limit = Number(req.query.limit ?? 50);
    const offset = Number(req.query.offset ?? 0);
    if (
      !Number.isSafeInteger(limit) ||
      limit < 1 ||
      limit > 200 ||
      !Number.isSafeInteger(offset) ||
      offset < 0
    ) {
      return res.status(400).json({ error: "历史分页参数无效。" });
    }
    if (!store.aggregatePage) await sync();
    try {
      res.json(await store.history(limit, offset));
    } catch {
      res.status(503).json({
        error: "数据库暂时无法读取，请稍后重试。",
        code: "DATABASE_FAILED",
      });
    }
  });
  app.get("/api/results", async (req, res) => {
    res.set("Cache-Control", "no-store");
    const id = req.query.snapshot;
    const aggregate = req.query.view === "aggregate";
    if (
      (req.query.view !== undefined && !aggregate) ||
      (aggregate && id !== undefined)
    )
      return res.status(400).json({ error: "检测结果视图无效。" });
    if (
      id !== undefined &&
      (typeof id !== "string" ||
        !/^[1-9]\d*$/.test(id) ||
        !Number.isSafeInteger(Number(id)))
    ) {
      return res.status(400).json({ error: "检测批次无效。" });
    }
    let query;
    try {
      query = parseResultsQuery(req.query);
    } catch {
      return res.status(400).json({ error: "节点分页、筛选或排序参数无效。" });
    }
    try {
      const databasePage = aggregate && query && store.aggregatePage;
      let sourceError = lastSourceError;
      if (!databasePage) {
        try {
          await store.sync(snapshotPath, sources);
          sourceError = undefined;
        } catch (error) {
          sourceError = error;
        }
      }
      const snapshot = databasePage
        ? await store.aggregatePage(query)
        : aggregate
          ? await store.aggregate()
          : await store.get(id === undefined ? undefined : Number(id));
      if (snapshot)
        return res.json({
          ...publicSnapshot(
            query && !databasePage
              ? paginateResults(snapshot, query)
              : snapshot,
          ),
          ippureCollector: collector?.status() || { enabled: false },
        });
      if (id !== undefined)
        return res
          .status(404)
          .json({ error: "该历史批次不存在。", code: "SNAPSHOT_NOT_FOUND" });
      if (!sourceError || sourceError.code === "ENOENT")
        return res.status(404).json({
          error: "尚未找到检测结果，请确认 subs-check 已完成至少一轮检测。",
          code: "NO_RESULTS",
        });
      throw sourceError;
    } catch {
      res.status(503).json({
        error: "检测结果暂时无法读取，请稍后刷新重试。",
        code: "READ_FAILED",
      });
    }
  });
  app.use("/api/downloads", (_req, res) =>
    res.status(404).json({ error: "订阅下载暂不可用。" }),
  );
  app.use("/api/download", (_req, res) =>
    res.status(404).json({ error: "订阅下载暂不可用。" }),
  );
  app.use("/api", (_req, res) =>
    res.status(404).json({ error: "接口不存在。" }),
  );
  if (distDir) {
    app.use(express.static(distDir));
    app.get("/{*path}", (_req, res) =>
      res.sendFile(path.join(distDir, "index.html")),
    );
  }
  return app;
}
