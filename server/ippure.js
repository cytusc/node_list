import { readFile } from "node:fs/promises";
import path from "node:path";
import { isIP } from "node:net";
import { parse } from "yaml";
import { aggregateIdentity } from "./node-sources.js";
import {
  queryIPPure,
  resolveExitIP,
  configureProxyBatch,
} from "./ippure-http.js";
export { queryIPPure, resolveExitIP } from "./ippure-http.js";

export function startIPPureCollector({
  store,
  subDir,
  controller,
  proxyUrl,
  secret,
  concurrency = 3,
  apiOrigin = "https://api.123169.xyz",
  configure = configureProxyBatch,
  query = queryIPPure,
  resolveIP = resolveExitIP,
  intervalMs = 30000,
}) {
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 10)
    throw new Error("IPPure 并发数必须是 1 到 10 的整数");
  const abort = new AbortController();
  let pending;
  let lastError = null;
  let rotation = 0;
  async function collect() {
    const snapshot = await store.get();
    if (!snapshot) return;
    const historical = store.ippureCandidates
      ? await store.ippureCandidates()
      : [];
    const aggregate = store.aggregate ? await store.aggregate() : undefined;
    const visible = new Set(
      (aggregate?.nodes || []).map(
        (node) => `${node.snapshotId}:${aggregateIdentity(node)}`,
      ),
    );
    historical.sort(
      (a, b) =>
        Number(visible.has(`${b.snapshotId}:${aggregateIdentity(b.node)}`)) -
        Number(visible.has(`${a.snapshotId}:${aggregateIdentity(a.node)}`)),
    );
    if (!snapshot.nodes.length && !historical.length) return;
    const document = snapshot.nodes.every((node) => node.rawConfig)
      ? { proxies: [] }
      : parse(await readFile(path.join(subDir, "output", "all.yaml"), "utf8"));
    if (!Array.isArray(document?.proxies))
      throw new Error("Clash 订阅缺少 proxies");
    const matchProxy = (node) => {
      if (node.rawConfig) return node.rawConfig;
      const matches = document.proxies.filter(
        (p) =>
          p.name === node.name &&
          p.type === node.type &&
          p.server === node.server &&
          String(p.port) === String(node.port),
      );
      return matches.length === 1 ? matches[0] : undefined;
    };
    // Query transport and the node receiving the result are independent.
    const working = (aggregate?.nodes || snapshot.nodes)
      .filter((node) => node.ippure?.status === "success")
      .map(matchProxy)
      .filter(Boolean);
    const unavailable = new Set();
    const ipCache = new Map();
    const asnCache = new Map();
    const jobs = [];
    const entries = [
      ...snapshot.nodes.map((node, index) => ({
        snapshotId: snapshot.id,
        index,
        node,
      })),
      ...historical.filter((item) => item.snapshotId !== snapshot.id),
    ];
    for (const { snapshotId, index, node } of entries) {
      if (abort.signal.aborted) return;
      const old = node.ippure;
      const currentExitIP = old?.exitIPSource === "ip.sb";
      const currentExitMode = old?.exitIPMode === "ip.sb";
      const currentSignature = old?.signatureVersion === 2;
      const currentResolution = old?.exitIPVersion === 2;
      const backfill =
        old?.status === "success" && currentExitIP && !old.asnOrganization;
      if (old?.status === "success" && currentExitIP && !backfill) continue;
      // Old failures used the target as transport. Give the new query mode
      // its own retry budget, including nodes that exhausted the old budget.
      const attempts =
        currentExitMode &&
        old?.queryMode === "specified-exit" &&
        old?.transportMode === "http" &&
        currentSignature &&
        currentResolution
          ? old.attempts || 0
          : 0;
      const organizationAttempts = currentSignature
        ? old.organizationAttempts || 0
        : 0;
      if ((backfill ? organizationAttempts : attempts) >= 3) continue;
      const lastAttempt = backfill
        ? currentSignature
          ? old.organizationCheckedAt
          : undefined
        : currentExitMode &&
            old?.queryMode === "specified-exit" &&
            old?.transportMode === "http" &&
            currentSignature &&
            currentResolution
          ? old.checkedAt
          : undefined;
      if (lastAttempt && Date.now() - Date.parse(lastAttempt) < 30 * 60 * 1000)
        continue;
      // A rewritten subscription may belong to a different detection round.
      const ownProxy = matchProxy(node);
      if (!ownProxy) continue;
      jobs.push({
        snapshotId,
        index,
        old,
        currentExitIP,
        backfill,
        attempts,
        ownProxy,
      });
    }
    for (let offset = 0; offset < jobs.length; offset += concurrency) {
      if (abort.signal.aborted || (await store.get())?.id !== snapshot.id)
        return;
      const batch = jobs.slice(offset, offset + concurrency);
      const donors = working.filter((proxy) => !unavailable.has(proxy));
      const pool =
        donors.length <= 3
          ? donors
          : Array.from(
              { length: 3 },
              (_, i) => donors[(rotation + i) % donors.length],
            );
      const proxies = [
        ...new Set([...batch.map((job) => job.ownProxy), ...pool]),
      ];
      const urls = await configure({
        controller,
        proxyUrl,
        secret,
        proxies,
        signal: abort.signal,
      });
      // A configuration reload happens only after all jobs in this batch finish.
      const outcomes = await Promise.allSettled(
        batch.map(
          async ({
            snapshotId,
            index,
            old,
            currentExitIP,
            backfill,
            attempts,
            ownProxy,
          }) => {
            const result = {
              checkedAt: new Date().toISOString(),
              attempts: attempts + 1,
              queryMode: "specified-exit",
              exitIPMode: "ip.sb",
              transportMode: "http",
              signatureVersion: 2,
              exitIPVersion: 2,
            };
            let failureStage = "exit-ip";
            try {
              const options = {
                controller,
                proxyUrl,
                secret,
                apiOrigin,
                asnCache,
                signal: abort.signal,
              };
              const target =
                currentExitIP &&
                typeof old?.ip === "string" &&
                isIP(old.ip) === 4
                  ? old.ip
                  : await resolveIP({
                      ...options,
                      proxyUrl: urls.get(ownProxy),
                      proxy: ownProxy,
                    });
              if (typeof target !== "string" || !isIP(target))
                throw new Error("IPPure 无法获取有效的节点出口 IP");
              result.ip = target;
              result.exitIPSource = "ip.sb";
              result.queryTarget = target;
              failureStage = "ippure-query";
              const available = [
                ...new Set([
                  ...pool,
                  ...working.filter((proxy) => proxies.includes(proxy)),
                ]),
              ].filter((proxy) => !unavailable.has(proxy));
              // Bootstrap the pool through a node itself when no usable donor exists.
              const candidates = available.length
                ? Array.from(
                    { length: Math.min(3, available.length) },
                    (_, offset) =>
                      available[(rotation + offset) % available.length],
                  )
                : [ownProxy];
              rotation++;
              const load = async () => {
                let failure;
                for (const proxy of candidates) {
                  try {
                    const data = await query({
                      controller,
                      proxyUrl: urls.get(proxy),
                      secret,
                      apiOrigin,
                      asnCache,
                      proxy,
                      target,
                      signal: abort.signal,
                    });

                    if (!working.includes(proxy)) working.push(proxy);
                    return data;
                  } catch (error) {
                    if (abort.signal.aborted) return;
                    unavailable.add(proxy);
                    failure = error;
                  }
                }
                throw failure || new Error("IPPure 没有可用查询代理");
              };
              let cached = ipCache.get(target);
              if (!cached) {
                cached = load();
                ipCache.set(target, cached);
                cached.catch(() => {
                  if (ipCache.get(target) === cached) ipCache.delete(target);
                });
              }
              Object.assign(result, await cached, { status: "success" });
            } catch (error) {
              if (abort.signal.aborted) return;
              if (backfill) {
                // Keep previously collected metrics if the organization refresh fails.
                if ((await store.get())?.id !== snapshot.id) return;
                await store.saveIPPure(snapshotId, index, {
                  ...old,
                  signatureVersion: 2,
                  organizationAttempts:
                    (old.signatureVersion === 2
                      ? old.organizationAttempts || 0
                      : 0) + 1,
                  organizationCheckedAt: result.checkedAt,
                });
                return;
              }
              result.status = "error";
              result.failureStage = failureStage;
              // Never persist proxy credentials or controller response bodies.
              result.error = /IPPure|IP\.SB|代理配置/.test(error.message)
                ? error.message.slice(0, 160)
                : failureStage === "exit-ip"
                  ? "节点 IPv4 出口探测失败，请检查节点连接后重试"
                  : "IPPure 查询代理连接失败，请稍后重试";
            }
            if (abort.signal.aborted || (await store.get())?.id !== snapshot.id)
              return;
            await store.saveIPPure(snapshotId, index, result);
          },
        ),
      );
      const failed = outcomes.find((outcome) => outcome.status === "rejected");
      if (failed) throw failed.reason;
    }
  }
  const run = () => {
    if (!pending)
      pending = collect()
        .then(() => {
          lastError = null;
        })
        .catch(() => {
          lastError =
            "IPPure 采集暂时不可用，请检查订阅文件、数据库与检测服务配置";
        })
        .finally(() => {
          pending = null;
        });
    return pending;
  };
  void run();
  const timer = setInterval(run, intervalMs);
  timer.unref();
  return {
    run,
    status: () => ({
      enabled: true,
      running: !!pending,
      concurrency,
      transport: "http",
      error: lastError,
    }),
    async close() {
      clearInterval(timer);
      abort.abort();
      await pending;
    },
  };
}
