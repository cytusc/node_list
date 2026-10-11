import express from "express";
import { stat } from "node:fs/promises";
import path from "node:path";
import { createStore } from "./store.js";

export const downloads = {
  clash: { file: "all.yaml", label: "Clash 节点", type: "application/yaml" },
  mihomo: {
    file: "mihomo.yaml",
    label: "Mihomo 配置",
    type: "application/yaml",
  },
  base64: { file: "base64.txt", label: "Base64 订阅", type: "text/plain" },
};

export function createApp({
  subDir,
  resultsFile,
  distDir,
  databasePath,
  store: configuredStore,
  importIntervalMs = 5000,
} = {}) {
  const app = express();
  app.disable("x-powered-by");
  app.get("/api/health", (_req, res) => res.json({ status: "ok" }));
  const snapshotPath =
    resultsFile ||
    path.join(subDir, "config", "cache", "config", "results.json");
  const store = configuredStore || createStore(databasePath);
  const sync = () => store.sync(snapshotPath).catch(() => {});
  void sync();
  const timer = setInterval(sync, importIntervalMs);
  timer.unref();
  app.locals.close = async () => {
    clearInterval(timer);
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
    await sync();
    try {
      res.json(await store.history(limit, offset));
    } catch {
      res
        .status(503)
        .json({
          error: "数据库暂时无法读取，请稍后重试。",
          code: "DATABASE_FAILED",
        });
    }
  });
  app.get("/api/results", async (req, res) => {
    res.set("Cache-Control", "no-store");
    const id = req.query.snapshot;
    if (
      id !== undefined &&
      (typeof id !== "string" ||
        !/^[1-9]\d*$/.test(id) ||
        !Number.isSafeInteger(Number(id)))
    ) {
      return res.status(400).json({ error: "检测批次无效。" });
    }
    try {
      let sourceError;
      try {
        await store.sync(snapshotPath);
      } catch (error) {
        sourceError = error;
      }
      const snapshot = await store.get(
        id === undefined ? undefined : Number(id),
      );
      if (snapshot) return res.json(snapshot);
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
  app.get("/api/downloads", async (_req, res) => {
    res.set("Cache-Control", "no-store");
    res.json(
      await Promise.all(
        Object.entries(downloads).map(async ([id, item]) => {
          try {
            const info = await stat(path.join(subDir, "output", item.file));
            return {
              id,
              label: item.label,
              available: info.isFile(),
              updatedAt: info.mtime.toISOString(),
            };
          } catch {
            return { id, label: item.label, available: false };
          }
        }),
      ),
    );
  });
  app.get("/api/download/:format", async (req, res) => {
    const item = Object.hasOwn(downloads, req.params.format)
      ? downloads[req.params.format]
      : null;
    if (!item) return res.status(404).json({ error: "不支持的订阅格式。" });
    res.set("Cache-Control", "no-store");
    res.download(path.join(subDir, "output", item.file), item.file, (error) => {
      if (error && !res.headersSent)
        res.status(404).json({
          error: "该订阅文件尚未生成，请等待 subs-check 完成检测与转换。",
        });
    });
  });
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
