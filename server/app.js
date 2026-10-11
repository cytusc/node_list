import express from "express";
import { readFile, stat } from "node:fs/promises";
import path from "node:path";

export const downloads = {
  clash: { file: "all.yaml", label: "Clash 节点", type: "application/yaml" },
  mihomo: {
    file: "mihomo.yaml",
    label: "Mihomo 配置",
    type: "application/yaml",
  },
  base64: { file: "base64.txt", label: "Base64 订阅", type: "text/plain" },
};

export function createApp({ subDir, resultsFile, distDir } = {}) {
  const app = express();
  app.disable("x-powered-by");
  const snapshotPath =
    resultsFile ||
    path.join(subDir, "config", "cache", "config", "results.json");
  app.get("/api/results", async (_req, res) => {
    res.set("Cache-Control", "no-store");
    try {
      const snapshot = JSON.parse(await readFile(snapshotPath, "utf8"));
      if (!Array.isArray(snapshot.nodes)) throw new Error("Invalid snapshot");
      // Whitelist the result schema; never expose arbitrary file contents or proxy credentials.
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
      res.json({
        checkedAt: snapshot.checkedAt,
        speedTest: !!snapshot.speedTest,
        mediaCheck: !!snapshot.mediaCheck,
        nodes: snapshot.nodes.map((n) =>
          Object.fromEntries(
            fields.filter((k) => k in n).map((k) => [k, n[k]]),
          ),
        ),
      });
    } catch (error) {
      if (error.code === "ENOENT")
        return res
          .status(404)
          .json({
            error: "尚未找到检测结果，请确认 subs-check 已完成至少一轮检测。",
            code: "NO_RESULTS",
          });
      res
        .status(503)
        .json({
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
        res
          .status(404)
          .json({
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
