import { readFile } from "node:fs/promises";
import path from "node:path";
import { isIP } from "node:net";
import { parse } from "yaml";
import { aggregateIdentity } from "./node-sources.js";
import {
  EXIT_IP_VERSION,
  EXIT_IP_PROBE_VERSION,
  hasVerifiedExitIP,
} from "./exit-ip.js";
import { shareLinkProxy, SHARE_LINK_VERSION } from "./share-link.js";
import {
  queryIPPure,
  resolveExitIPInfo,
  resolveExitCountry,
  configureProxyBatch,
} from "./ippure-http.js";
import { probeGitHub } from "./github-patrol.js";
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
  resolveCountry = resolveExitCountry,
  githubProbe = probeGitHub,
  intervalMs = 30000,
}) {
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 10)
    throw new Error("IPPure 并发数必须是 1 到 10 的整数");
  const abort = new AbortController();
  let pending;
  let lastError = null;
  let rotation = 0;
  async function collect() {
    const context = store.getCollectionContext
      ? await store.getCollectionContext()
      : {
          snapshot:
            (await store.getCollectionSnapshot?.()) ?? (await store.get()),
          historical: store.ippureCandidates
            ? await store.ippureCandidates()
            : [],
          aggregate: store.aggregate ? await store.aggregate() : undefined,
        };
    const snapshot = context.snapshot;
    if (!snapshot) return;
    const historical = context.historical || [];
    const aggregate = context.aggregate;
    const visible = new Set(
      (aggregate?.nodes || []).map(
        (node) => `${node.snapshotId}:${aggregateIdentity(node)}`,
      ),
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
    // Echo reachability is a per-exit property; remembering the winning
    // endpoint spares known-good nodes the full batch sweep on later passes.
    const preferredEndpoints = new Map();
    // Retiring a dead node requires proof the probe infrastructure itself
    // works: any successful result in this run provides that control.
    let healthyRun = false;
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
      entry.visibleRank = Number(
        visible.has(`${snapshotId}:${entry.identity}`),
      );
      if (!entry.shareOnly) {
        entry.priority = 0;
        continue;
      }
      const saved = aggregateResults.get(entry.identity) || node.ippure;
      const outdatedLink =
        saved?.failureStage === "share-link" &&
        saved.shareLinkVersion !== SHARE_LINK_VERSION;
      const outdatedProbe =
        saved?.status === "error" &&
        !saved.ip &&
        saved.failureStage !== "share-link" &&
        saved.exitIPProbeVersion !== EXIT_IP_PROBE_VERSION;
      entry.priority =
        (outdatedLink || outdatedProbe ? 4 : !saved ? 2 : 0) +
        entry.visibleRank;
    }
    // Probe each connection once per run and prioritize never-checked visible
    // nodes. Within an equal (priority, visibility), the oldest identity goes
    // first: raw snapshot index order structurally starves nodes parked near
    // the tail of every batch (small country groups never reach the per-run
    // probe budget — the AE nodes pending since their first import).
    const firstSeen = new Map();
    for (const { identity, snapshotId } of entries)
      firstSeen.set(
        identity,
        Math.min(firstSeen.get(identity) ?? Infinity, snapshotId),
      );
    // Departed panel members are materialized in node_explorer_departed_nodes
    // by the projection worker; probing them only burns the budget that live
    // nodes need, and their historical results are already on file.
    const departed = store.departedNodeIdentities
      ? await store.departedNodeIdentities()
      : new Set();
    entries.sort(
      (a, b) =>
        b.priority - a.priority ||
        b.visibleRank - a.visibleRank ||
        firstSeen.get(a.identity) - firstSeen.get(b.identity),
    );
    const seen = new Set();
    for (const { snapshotId, index, node, identity, shareOnly } of entries) {
      if (abort.signal.aborted) return;
      if (departed.has(identity)) continue;
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
      const currentProbe =
        old?.status !== "error" ||
        !!old.ip ||
        old.exitIPProbeVersion === EXIT_IP_PROBE_VERSION;
      const currentRetry =
        currentExitMode &&
        old?.queryMode === "specified-exit" &&
        old?.transportMode === "http" &&
        currentSignature &&
        currentResolution &&
        currentProbe;
      const backfill =
        old?.status === "success" && currentExitIP && !old.asnOrganization;
      // Success results verified before country collection existed re-enter
      // the queue once to attach the exit country; the throttles below keep
      // a persistently unresolvable node from looping at probe frequency.
      const needsCountry =
        old?.status === "success" && currentExitIP && !old.country;
      if (
        old?.status === "success" &&
        currentExitIP &&
        !backfill &&
        !needsCountry
      )
        continue;
      // Old failures used the target as transport. Give the new query mode
      // its own retry budget, including nodes that exhausted the old budget.
      const attempts = currentRetry ? old.attempts || 0 : 0;
      const lastAttempt = backfill
        ? currentSignature
          ? old.organizationCheckedAt
          : undefined
        : currentRetry
          ? old.checkedAt
          : undefined;
      // Every exit-IP probe burns the full multi-provider timeout budget.
      // Nodes that keep failing before any exit IP is observed escalate to
      // a 6-hour and then a one-day backoff, so a backlog of dead nodes
      // cannot starve never-checked ones out of the limited probe windows.
      const exitProbeFailures =
        old?.status === "error" && !old.ip && old.failureStage === "exit-ip";
      const backoff = exitProbeFailures
        ? attempts >= 3
          ? 24 * 60 * 60 * 1000
          : attempts >= 2
            ? 6 * 60 * 60 * 1000
            : 30 * 60 * 1000
        : 30 * 60 * 1000;
      if (lastAttempt && Date.now() - Date.parse(lastAttempt) < backoff)
        continue;
      // A rewritten subscription may belong to a different detection round.
      const ownProxy = matchProxy(node);
      if (!ownProxy) {
        // Persist why the node cannot be probed; otherwise sources that stop
        // exposing share links leave their nodes as 待检测 forever. The
        // share-link stage + current version keeps marked instances from
        // re-entering every round.
        await store.saveIPPure(
          snapshotId,
          index,
          {
            status: "unsupported",
            checkedAt: new Date().toISOString(),
            attempts: 0,
            failureStage: "share-link",
            shareLinkVersion: SHARE_LINK_VERSION,
            error: node.rawLink
              ? "节点分享链接格式或连接参数不受当前检测服务支持"
              : "节点缺少可用的连接配置（分享链接为空），无法构造检测通道",
          },
          identity,
        );
        continue;
      }
      jobs.push({
        identity,
        snapshotId,
        index,
        node,
        old,
        currentExitIP,
        backfill,
        attempts,
        ownProxy,
        shareOnly,
      });
    }
    // One Mihomo reload per wave of concurrency * 3 jobs amortizes the
    // configuration cost; jobs inside a wave no longer wait at a barrier.
    // The queue drains across snapshot changes: results are keyed to each
    // entry's own (snapshotId, index), so a newer snapshot never invalidates
    // pending work, and retirement is re-checked at every wave boundary.
    const waveSize = concurrency * 3;
    for (let offset = 0; offset < jobs.length; offset += waveSize) {
      if (abort.signal.aborted) return;
      const retired = store.retiredNodeIdentities
        ? await store.retiredNodeIdentities()
        : new Set();
      let waveJobs = jobs
        .slice(offset, offset + waveSize)
        .filter((job) => !retired.has(job.identity));
      if (!waveJobs.length) continue;
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
        ...new Set([...waveJobs.map((job) => job.ownProxy), ...pool]),
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
        // wave from being detected. Isolate only explicit configuration errors;
        // controller outages retain the collector's normal retry behavior.
        if (
          !/代理配置失败 \(HTTP (400|422)\)/.test(error.message) ||
          !waveJobs.some((job) => job.shareOnly)
        )
          throw error;
        const valid = [];
        for (const job of waveJobs) {
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
            await store.saveIPPure(
              job.snapshotId,
              job.index,
              {
                status: "unsupported",
                checkedAt: new Date().toISOString(),
                attempts: 0,
                failureStage: "share-link",
                shareLinkVersion: SHARE_LINK_VERSION,
                error: "节点分享链接配置被当前检测服务拒绝",
              },
              job.identity,
            );
          }
        }
        waveJobs = valid;
        if (!waveJobs.length) continue;
        proxies = [
          ...new Set([...waveJobs.map((job) => job.ownProxy), ...pool]),
        ];
        urls = await configureBatch(proxies);
      }
      // The window starts one job per freed slot instead of waiting for the
      // whole wave; a rejection still fails the run, but only after every
      // started job has drained.
      const runJob = async ({
        snapshotId,
        index,
        node,
        identity,
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
          exitIPProbeVersion: EXIT_IP_PROBE_VERSION,
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
          const reuseExit =
            currentExitIP &&
            typeof old?.ip === "string" &&
            isIP(old.ip) === 4;
          const resolved = reuseExit
            ? {
                ip: old.ip,
                provider: old.exitIPSource,
                country: old.country,
              }
            : await resolveIP({
                ...options,
                proxyUrl: urls.get(ownProxy),
                proxy: ownProxy,
                preferredEndpoints: preferredEndpoints.get(identity) || [],
                onProbeAttempt: (attempt) => {
                  (result.exitIPAttempts ||= []).push(attempt);
                },
              });
          // Verified exits probed before country collection keep their IP
          // without a location; one trace request over the node's own
          // transport fills it in. Best effort: a dead node retries on
          // the normal backoff schedule instead of blocking the queue.
          if (reuseExit && !resolved.country) {
            try {
              resolved.country = await resolveCountry({
                ...options,
                proxyUrl: urls.get(ownProxy),
              });
            } catch {
              /* retried on the next scheduled pass */
            }
          }
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
          if (resolved?.endpoint)
            preferredEndpoints.set(identity, resolved.endpoint);
          if (resolved?.country) result.country = resolved.country;
          result.addressFamily = isIP(target);
          result.queryTarget = target;
          if (result.addressFamily === 6) {
            result.status = "unsupported";
            result.failureStage = "unsupported-ipv6";
            result.error =
              "已获取 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6";
            if (abort.signal.aborted) return;
            await store.saveIPPure(snapshotId, index, result, identity);
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
                    waveJobs.find((job) => job.ownProxy === proxy)?.identity,
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
          healthyRun = true;
        } catch (error) {
          if (abort.signal.aborted) return;
          if (backfill) {
            // Keep previously collected metrics if the organization refresh fails.
            await store.saveIPPure(
              snapshotId,
              index,
              {
                ...old,
                signatureVersion: 2,
                organizationAttempts:
                  (old.signatureVersion === 2
                    ? old.organizationAttempts || 0
                    : 0) + 1,
                organizationCheckedAt: result.checkedAt,
              },
              identity,
            );
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
            result.error =
              error.exitIPFailure ||
              /IPPure|IP\.SB|代理配置/.test(error.message)
                ? error.message.slice(0, 160)
                : failureStage === "exit-ip"
                  ? "节点 IPv4 出口探测失败，请检查节点连接后重试"
                  : "IPPure 查询代理连接失败，请稍后重试";
          }
        }
        // Results are keyed to the entry's own immutable (snapshotId, index);
        // a newer snapshot arriving mid-run never invalidates them.
        if (abort.signal.aborted) return;
        await store.saveIPPure(snapshotId, index, result, identity);
        // Three fast retries plus a one-day-later recheck that still
        // cannot yield any exit IP means the node is dead; retire it so
        // it leaves both the display and the collection queue. A later
        // patrol recheck restores revived nodes.
        if (
          result.status === "error" &&
          result.failureStage === "exit-ip" &&
          result.attempts >= 4 &&
          healthyRun
        ) {
          // Echo endpoints can be blocked by the node's own egress while the
          // transport is fine. Corroborate through the node's own tunnel with
          // a GitHub probe before retiring, otherwise reachable nodes get
          // deleted and later revived in a churn loop by the patrol.
          let transportAlive = false;
          try {
            transportAlive = await githubProbe({
              proxyUrl: urls.get(ownProxy),
              timeoutMs: 12000,
              signal: abort.signal,
            });
          } catch (error) {
            if (abort.signal.aborted) return;
          }
          if (transportAlive) {
            await store.saveIPPure(
              snapshotId,
              index,
              { ...result, githubReachable: true },
              identity,
            );
          } else {
            await store.retireNodes?.(
              [node],
              `IPPure: exit-IP probe failed ${result.attempts} times`,
            );
          }
        }
      };
      // Sliding window: keep `concurrency` jobs in flight, starting the next
      // one as soon as a slot frees up instead of waiting at a wave barrier.
      let next = 0;
      let rejection;
      const worker = async () => {
        while (next < waveJobs.length) {
          const job = waveJobs[next++];
          // Retirements can happen while earlier wave jobs run; recheck
          // before spending a probe window on an already retired node.
          if (
            store.retiredNodeIdentities &&
            (await store.retiredNodeIdentities()).has(job.identity)
          )
            continue;
          try {
            await runJob(job);
          } catch (error) {
            rejection ??= error;
          }
        }
      };
      await Promise.all(
        Array.from({ length: Math.min(concurrency, waveJobs.length) }, worker),
      );
      if (rejection) throw rejection;
    }
  }
  const run = () => {
    if (!pending)
      pending = collect()
        .then(() => {
          lastError = null;
        })
        .catch((error) => {
          // Expose the cause without persisting proxy credentials or
          // controller response bodies that can ride on other messages.
          const message = error?.message || "";
          lastError = /IPPure|IP\.SB|代理配置|timeout/i.test(message)
            ? `IPPure 采集暂时不可用：${message.slice(0, 160)}`
            : "IPPure 采集暂时不可用，请检查订阅文件、数据库与检测服务配置";
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
