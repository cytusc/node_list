import { fetch as proxyFetch, ProxyAgent } from "undici";
import { setTimeout as delay } from "node:timers/promises";
import { configureProxyBatch } from "./ippure-http.js";
import { aggregateIdentity } from "./node-sources.js";

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
  for (let offset = 0; offset < nodes.length; offset += concurrency) {
    signal.throwIfAborted();
    const retired = await store.retiredNodeIdentities();
    const batch = nodes.slice(offset, offset + concurrency).filter((node) => {
      if (retired.has(aggregateIdentity(node)) || !node.rawConfig) {
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
    const proxies = targets.map((node) => node.rawConfig);
    const urls = await configure({
      controller,
      proxyUrl,
      secret,
      proxies,
      signal,
    });
    const check = (node) => {
      const url = urls.get(node.rawConfig);
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
