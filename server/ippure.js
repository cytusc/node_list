import { readFile } from "node:fs/promises";
import path from "node:path";
import { isIP } from "node:net";
import { parse } from "yaml";
import { aggregateIdentity } from "./node-sources.js";
import { EXIT_IP_VERSION, hasVerifiedExitIP } from "./exit-ip.js";
import { shareLinkProxy, SHARE_LINK_VERSION } from "./share-link.js";
import {
  queryIPPure,
  resolveExitIPInfo,
  configureProxyBatch,
} from "./ippure-http.js";
export {
  queryIPPure,
  resolveExitIP,
  resolveExitIPInfo,
} from "./ippure-http.js";

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
  resolveIP = resolveExitIPInfo,
  intervalMs = 30000,
}) {
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 10)
    throw new Error("IPPure 并发数必须是 1 到 10 的整数");
  const abort = new AbortController();
  let pending;
  let lastError = null;
  let rotation = 0;
  async function collect() {
    const snapshot = store.getCollectionSnapshot
      ? await store.getCollectionSnapshot()
      : await store.get();
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
    const document = snapshot.nodes.every(
      (node) => node.rawConfig || node.rawLink,
    )
      ? { proxies: [] }
      : parse(await readFile(path.join(subDir, "output", "all.yaml"), "utf8"));
    if (!Array.isArray(document?.proxies))
      throw new Error("Clash 订阅缺少 proxies");
    const parsedLinks = new WeakMap();
    const matchProxy = (node) => {
      if (node.rawConfig) return node.rawConfig;
      if (node.rawLink) {
        if (!parsedLinks.has(node)) {
          try {
            parsedLinks.set(node, shareLinkProxy(node.rawLink, node.name));
          } catch {
            parsedLinks.set(node, undefined);
          }
        }
        return parsedLinks.get(node);
      }
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
    const transportIdentities = new Map(
      (aggregate?.nodes || snapshot.nodes).map((node) => [
        matchProxy(node),
        aggregateIdentity(node),
      ]),
    );
    const unavailable = new Set();
    const ipCache = new Map();
    const asnCache = new Map();
    const jobs = [];
    const aggregateResults = new Map(
      (aggregate?.nodes || []).map((node) => [
        aggregateIdentity(node),
        node.ippure,
      ]),
    );
    const entries = [
      ...snapshot.nodes.map((node, index) => ({
        snapshotId: snapshot.id,
        index: snapshot.nodeIndexes?.[index] ?? index,
        node,
      })),
      ...historical.filter((item) => item.snapshotId !== snapshot.id),
    ];
    // Cache identity once; recomputing share-link hashes inside the sort
    // comparator makes larger histories block the event loop.
    for (const entry of entries) {
      const { node, snapshotId } = entry;
      entry.identity = aggregateIdentity(node);
      entry.shareOnly = !node.rawConfig && !!node.rawLink;
      if (!entry.shareOnly) {
        entry.priority = 0;
        continue;
      }
      const saved = aggregateResults.get(entry.identity) || node.ippure;
      const outdatedLink =
        saved?.failureStage === "share-link" &&
        saved.shareLinkVersion !== SHARE_LINK_VERSION;
      entry.priority =
        (outdatedLink ? 4 : !saved ? 2 : 0) +
        Number(visible.has(`${snapshotId}:${entry.identity}`));
    }
    // New source batches must not restart probes at the same first few nodes.
    // Probe each connection once per run and prioritize never-checked visible nodes.
    entries.sort((a, b) => b.priority - a.priority);
    const seen = new Set();
    for (const { snapshotId, index, node, identity, shareOnly } of entries) {
      if (abort.signal.aborted) return;
      if (shareOnly && seen.has(identity)) continue;
      if (shareOnly) seen.add(identity);
      const old = (shareOnly && aggregateResults.get(identity)) || node.ippure;
      if (
        old?.failureStage === "share-link" &&
        old.shareLinkVersion === SHARE_LINK_VERSION
      )
        continue;
      const currentExitIP = hasVerifiedExitIP(old);
      const currentExitMode = old?.exitIPMode === "proxy-echo";
      const currentSignature = old?.signatureVersion === 2;
      const currentResolution = old?.exitIPVersion === EXIT_IP_VERSION;
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
      if (!ownProxy) {
        if (node.rawLink)
          await store.saveIPPure(snapshotId, index, {
            status: "unsupported",
            checkedAt: new Date().toISOString(),
            attempts: 0,
            failureStage: "share-link",
            shareLinkVersion: SHARE_LINK_VERSION,
            error: "节点分享链接格式或连接参数不受当前检测服务支持",
          });
        continue;
      }
      jobs.push({
        identity: aggregateIdentity(node),
        snapshotId,
        index,
        old,
        currentExitIP,
        backfill,
        attempts,
        ownProxy,
        shareOnly,
      });
    }
    for (let offset = 0; offset < jobs.length; offset += concurrency) {
      if (abort.signal.aborted || (await store.get())?.id !== snapshot.id)
        return;
      const retired = store.retiredNodeIdentities
        ? await store.retiredNodeIdentities()
        : new Set();
      let batch = jobs
        .slice(offset, offset + concurrency)
        .filter((job) => !retired.has(job.identity));
      if (!batch.length) continue;
      const donors = working.filter(
        (proxy) =>
          !unavailable.has(proxy) &&
          !retired.has(transportIdentities.get(proxy)),
      );
      const pool =
        donors.length <= 3
          ? donors
          : Array.from(
              { length: 3 },
              (_, i) => donors[(rotation + i) % donors.length],
            );
      let proxies = [
        ...new Set([...batch.map((job) => job.ownProxy), ...pool]),
      ];
      const configureBatch = (selected) =>
        configure({
          controller,
          proxyUrl,
          secret,
          proxies: selected,
          signal: abort.signal,
        });
      let urls;
      try {
        urls = await configureBatch(proxies);
      } catch (error) {
        // A malformed archived link must not prevent every other node in the
        // batch from being detected. Isolate only explicit configuration errors;
        // controller outages retain the collector's normal retry behavior.
        if (
          !/代理配置失败 \(HTTP (400|422)\)/.test(error.message) ||
          !batch.some((job) => job.shareOnly)
        )
          throw error;
        const valid = [];
        for (const job of batch) {
          if (!job.shareOnly) {
            valid.push(job);
            continue;
          }
          try {
            await configureBatch([job.ownProxy]);
            valid.push(job);
          } catch (failure) {
            if (!/代理配置失败 \(HTTP (400|422)\)/.test(failure.message))
              throw failure;
            await store.saveIPPure(job.snapshotId, job.index, {
              status: "unsupported",
              checkedAt: new Date().toISOString(),
              attempts: 0,
              failureStage: "share-link",
              shareLinkVersion: SHARE_LINK_VERSION,
              error: "节点分享链接配置被当前检测服务拒绝",
            });
          }
        }
        batch = valid;
        if (!batch.length) continue;
        proxies = [...new Set([...batch.map((job) => job.ownProxy), ...pool])];
        urls = await configureBatch(proxies);
      }
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
              exitIPMode: "proxy-echo",
              transportMode: "http",
              signatureVersion: 2,
              exitIPVersion: EXIT_IP_VERSION,
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
              const resolved =
                currentExitIP &&
                typeof old?.ip === "string" &&
                isIP(old.ip) === 4
                  ? { ip: old.ip, provider: old.exitIPSource }
                  : await resolveIP({
                      ...options,
                      proxyUrl: urls.get(ownProxy),
                      proxy: ownProxy,
                    });
              // Injected legacy resolvers return a string; production returns
              // the address and the service that actually observed it.
              const { ip: target, provider } =
                typeof resolved === "string"
                  ? { ip: resolved, provider: "ip.sb" }
                  : resolved || {};
              if (typeof target !== "string" || !isIP(target))
                throw new Error("IPPure 无法获取有效的节点出口 IP");
              result.ip = target;
              result.exitIPSource = provider;
              result.addressFamily = isIP(target);
              result.queryTarget = target;
              if (result.addressFamily === 6) {
                result.status = "unsupported";
                result.failureStage = "unsupported-ipv6";
                result.error =
                  "已获取 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6";
                if (
                  abort.signal.aborted ||
                  (await store.get())?.id !== snapshot.id
                )
                  return;
                await store.saveIPPure(snapshotId, index, result);
                return;
              }
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
                    if (!transportIdentities.has(proxy))
                      transportIdentities.set(
                        proxy,
                        batch.find((job) => job.ownProxy === proxy)?.identity,
                      );
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
              if (
                failureStage === "exit-ip" &&
                old?.status === "unsupported" &&
                currentExitIP &&
                isIP(old.ip || "") === 6
              ) {
                // A later unreachable echo service does not erase an observed
                // IPv6 exit or turn IPv4 scoring support into node failure.
                Object.assign(result, {
                  status: "unsupported",
                  ip: old.ip,
                  exitIPSource: old.exitIPSource,
                  addressFamily: 6,
                  failureStage: "unsupported-ipv6",
                  error: old.error,
                });
              } else {
                result.status = "error";
                result.failureStage = failureStage;
                // Never persist proxy credentials or controller response bodies.
                result.error = /IPPure|IP\.SB|代理配置/.test(error.message)
                  ? error.message.slice(0, 160)
                  : failureStage === "exit-ip"
                    ? "节点 IPv4 出口探测失败，请检查节点连接后重试"
                    : "IPPure 查询代理连接失败，请稍后重试";
              }
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
