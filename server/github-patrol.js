import { fetch as proxyFetch, ProxyAgent } from "undici";
import { setTimeout as delay } from "node:timers/promises";
import { configureProxyBatch } from "./ippure-http.js";
import { aggregateIdentity } from "./node-sources.js";
import { shareLinkProxy } from "./share-link.js";

// A fresh dispatcher per attempt prevents a dead tunnel being reused on retry.
export async function probeGitHub({ proxyUrl, timeoutMs, signal }) {
  const dispatcher = proxyUrl ? new ProxyAgent(proxyUrl) : undefined;
  try {
    const response = await proxyFetch("https://github.com/", {
      ...(dispatcher ? { dispatcher } : {}),
      redirect: "manual",
      headers: { "User-Agent": "node-explorer-github-patrol" },
      signal: AbortSignal.any([signal, AbortSignal.timeout(timeoutMs)]),
    });
    await response.body?.cancel();
    return response.status === 200;
  } catch {
    signal.throwIfAborted();
    return false;
  } finally {
    await dispatcher?.destroy();
  }
}

export async function patrolOnce({
  store,
  controller,
  proxyUrl,
  secret,
  concurrency = 2,
  timeoutMs = 12000,
  retryDelayMs = 10000,
  signal,
  configure = configureProxyBatch,
  probe = probeGitHub,
  onProgress = async () => {},
}) {
  const summary = {
    total: 0,
    checked: 0,
    reachable: 0,
    retired: 0,
    deferred: 0,
    skipped: 0,
  };
  const nodes = (await store.aggregate())?.nodes || [];
  summary.total = nodes.length;
  // Only a node that actually passes this run can serve as a proxy control.
  let reference;
  nodes.sort(
    (a, b) =>
      Number(b.ippure?.status === "success") -
      Number(a.ippure?.status === "success"),
  );
  const pending = [];
  // Keep converted configurations separate: adding rawConfig to a share-only
  // node would change its stored connection identity and break soft deletion.
  const configs = new Map();
  const configureTargets = (targets) =>
    configure({
      controller,
      proxyUrl,
      secret,
      proxies: targets.map((node) => configs.get(node)),
      signal,
    });
  const rejectedConfig = (error) =>
    /代理配置失败 \(HTTP (400|422)\)/.test(error.message);
  for (let offset = 0; offset < nodes.length; offset += concurrency) {
    signal.throwIfAborted();
    const retired = await store.retiredNodeIdentities();
    let batch = nodes.slice(offset, offset + concurrency).filter((node) => {
      if (retired.has(aggregateIdentity(node))) {
        summary.skipped++;
        return false;
      }
      try {
        const config =
          node.rawConfig ||
          (node.rawLink && shareLinkProxy(node.rawLink, node.name));
        if (!config) throw new Error("Missing connection configuration");
        configs.set(node, config);
      } catch {
        summary.skipped++;
        return false;
      }
      return true;
    });
    if (!batch.length) continue;
    if (reference && retired.has(aggregateIdentity(reference)))
      reference = null;
    const targets = [...batch];
    if (reference && !targets.includes(reference)) targets.push(reference);
    let urls;
    try {
      urls = await configureTargets(targets);
    } catch (error) {
      if (!rejectedConfig(error)) throw error;
      // One unsupported configuration must not block the remaining nodes.
      const valid = [];
      for (const node of targets) {
        try {
          await configureTargets([node]);
          valid.push(node);
        } catch (failure) {
          if (!rejectedConfig(failure)) throw failure;
          if (batch.includes(node)) summary.skipped++;
          if (reference === node) reference = null;
        }
      }
      batch = batch.filter((node) => valid.includes(node));
      if (!batch.length) continue;
      urls = await configureTargets(valid);
    }
    const check = (node) => {
      const url = urls.get(configs.get(node));
      if (!url) throw new Error("巡检缺少节点隔离代理入口");
      return probe({
        proxyUrl: url,
        timeoutMs,
        signal,
      });
    };
    const first = await Promise.all(batch.map(check));
    const failures = batch.filter((_, i) => !first[i]);
    summary.checked += batch.length;
    summary.reachable += first.filter(Boolean).length;
    let good = batch.find((_, i) => first[i]);
    if (failures.length) {
      await delay(retryDelayMs, undefined, { signal });
      const second = await Promise.all(failures.map(check));
      summary.reachable += second.filter(Boolean).length;
      good ||= failures.find((_, i) => second[i]);
      const confirmed = failures.filter((_, i) => !second[i]);
      // Verify both direct host connectivity and a working proxy after failures.
      // Outages in GitHub, the host, DNS, or Mihomo must not retire the fleet.
      const control = good || reference;
      const hostOK = confirmed.length && (await probe({ timeoutMs, signal }));
      const proxyOK = hostOK && control && (await check(control));
      if (proxyOK) {
        summary.retired += await store.retireNodes(
          confirmed,
          "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed",
        );
      } else {
        pending.push(...confirmed);
      }
    }
    if (good) reference = good;
    summary.deferred = pending.length;
    await onProgress({ ...summary });
  }
  // Without a working control, defer deletion until a future scheduled run.
  summary.deferred = pending.length;
  await onProgress({ ...summary });
  return summary;
}

// State is persisted in the existing patrol status file, without a schema change.
export async function recoverRetiredNodes({
  store,
  controller,
  proxyUrl,
  secret,
  signal,
  state = {},
  now = Date.now(),
  intervalMs = 7200000,
  timeoutMs = 12000,
  configure = configureProxyBatch,
  probe = probeGitHub,
  onProgress = async () => {},
}) {
  const reason =
    "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed";
  // IPPure retirements go through the same revival cycle: a node that passes
  // the GitHub probe twice has its transport back and deserves a recheck.
  const retired = (await store.retiredNodes()).filter(
    (row) => row.reason === reason || row.reason.startsWith("IPPure:"),
  );
  const identities = new Set(retired.map((row) => row.identity));
  for (const identity of Object.keys(state))
    if (!identities.has(identity)) delete state[identity];
  const due = retired.filter((row) => {
    const previous = state[row.identity];
    const last =
      previous?.retiredAt === row.retiredAt
        ? Date.parse(previous.checkedAt)
        : Date.parse(row.retiredAt);
    return now - last >= intervalMs;
  });
  const summary = { checked: 0, restored: 0, skipped: 0 };
  if (!due.length) return summary;
  const nodes = new Map(
    ((await store.aggregate({ includeRetired: true }))?.nodes || []).map(
      (node) => [aggregateIdentity(node), node],
    ),
  );
  for (const row of due) {
    signal.throwIfAborted();
    const node = nodes.get(row.identity);
    let config;
    try {
      config =
        node?.rawConfig ||
        (node?.rawLink && shareLinkProxy(node.rawLink, node.name));
    } catch {
      /* Unsupported archived link. */
    }
    if (!config) {
      summary.skipped++;
      continue;
    }
    let urls;
    try {
      urls = await configure({
        controller,
        proxyUrl,
        secret,
        proxies: [config],
        signal,
      });
    } catch (error) {
      if (!/代理配置失败 \(HTTP (400|422)\)/.test(error.message)) throw error;
      summary.skipped++;
      continue;
    }
    const url = urls.get(config);
    if (!url) throw new Error("复检缺少节点隔离代理入口");
    const success = await probe({ proxyUrl: url, timeoutMs, signal });
    const previous = state[row.identity];
    const successes = success
      ? (previous?.retiredAt === row.retiredAt ? previous.successes || 0 : 0) +
        1
      : 0;
    state[row.identity] = {
      retiredAt: row.retiredAt,
      checkedAt: new Date(now).toISOString(),
      successes,
      success,
    };
    summary.checked++;
    if (successes >= 2) {
      summary.restored += await store.restoreNode(
        row.identity,
        row.retiredAt,
        row.reason,
      );
      delete state[row.identity];
    }
    await onProgress({ ...summary });
  }
  return summary;
}
