import { readFile } from "node:fs/promises";
import { aggregateIdentity } from "./node-sources.js";
import { createFeishuNotifier } from "./feishu-notify.js";

const baseUrl = "https://www.qilan.de";
const pageSize = 200;
// A member round can never need more than this; the cap turns a hostile or
// looping upstream from an infinite crawl into a failed refresh.
const maxPages = 60;
// Two extra attempts for a short page: upstream throttling answers with a
// partial page rather than an error, so a single short page is a transport
// symptom, not the end of the member list.
const attemptsPerPage = 3;
// Tolerate a few rows vanishing between the count and the last page; anything
// larger means the crawl was truncated and the round must not be published.
const missingTolerance = (total) => Math.max(5, total * 0.05);

function linkSettings(link) {
  try {
    if (link.startsWith("vmess://")) {
      const config = JSON.parse(
        Buffer.from(link.slice("vmess://".length), "base64").toString("utf8"),
      );
      return {
        tls: Boolean(config.tls),
        sni: config.sni || config.host || "",
      };
    }
    const url = new URL(link);
    const type = url.protocol.slice(0, -1);
    return {
      tls:
        ["trojan", "hy2", "hysteria2", "anytls"].includes(type) ||
        ["tls", "reality"].includes(url.searchParams.get("security")),
      sni:
        url.searchParams.get("sni") ||
        url.searchParams.get("serverName") ||
        "",
    };
  } catch {
    return { tls: false, sni: "" };
  }
}

function normalizeNode(node) {
  if (
    !node ||
    typeof node !== "object" ||
    !node.id ||
    !node.protocol ||
    !node.server ||
    !node.port ||
    typeof node.link !== "string"
  )
    return null;
  const name = String(node.name || `Qilan ${node.id}`);
  const settings = linkSettings(node.link);
  const details = [
    { k: "source-id", v: String(node.id) },
    Number.isFinite(Number(node.ping)) && Number(node.ping) > 0
      ? { k: "latency", v: `${Number(node.ping)} ms` }
      : null,
    node.status ? { k: "status", v: String(node.status) } : null,
    node.ip_type ? { k: "ip-type", v: String(node.ip_type) } : null,
    node.isp ? { k: "isp", v: String(node.isp) } : null,
    node.asn ? { k: "asn", v: String(node.asn) } : null,
    Array.isArray(node.features_list) && node.features_list.length
      ? { k: "features", v: node.features_list.join(", ") }
      : null,
  ].filter(Boolean);
  return {
    name: `${name} [Qilan]`,
    baseName: name,
    type: String(node.protocol).toLowerCase(),
    server: String(node.server),
    port: String(node.port),
    sni: settings.sni || undefined,
    tls: settings.tls,
    udp: false,
    network: String(node.network || "tcp").toLowerCase(),
    speed: 0,
    country: node.region ? String(node.region) : undefined,
    subTag: "Qilan",
    details,
    rawLink: node.link,
  };
}

export async function fetchQilanSnapshot({
  session,
  userAgent = "Mozilla/5.0",
  fetchImpl = fetch,
  sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
} = {}) {
  if (!session?.trim()) throw new Error("Qilan session cookie is empty");
  const nodes = new Map();
  let page = 1;
  let total;
  let newestCheck = 0;
  let received = 0;
  while (true) {
    let items;
    for (let attempt = 0; attempt < attemptsPerPage; attempt++) {
      const url = new URL("/api/nodes", baseUrl);
      url.searchParams.set("page", String(page));
      url.searchParams.set("limit", String(pageSize));
      const response = await fetchImpl(url, {
        headers: {
          Accept: "application/json",
          Referer: `${baseUrl}/`,
          "User-Agent": userAgent,
          Cookie: `session=${session.trim()}`,
        },
        signal: AbortSignal.timeout(30_000),
      });
      if (response.status === 403)
        throw new Error("Qilan rejected the session cookie (HTTP 403)");
      if (!response.ok)
        throw new Error(`Qilan request failed (HTTP ${response.status})`);
      const payload = await response.json();
      if (!Array.isArray(payload?.data))
        throw new Error("Qilan returned an invalid node page");
      if (Number.isFinite(Number(payload.total))) total = Number(payload.total);
      items = payload.data;
      if (items.length && !items.some((entry) => typeof entry?.link === "string" && entry.link.trim()))
        throw new Error(
          "Qilan session rejected: nodes returned without share links (stale session?)",
        );
      // Upstream throttling answers with fewer rows than requested while still
      // reporting the full total. Expect the smaller of a full page and what is
      // left, recomputed per response because `total` only arrives with it.
      const expected =
        total === undefined
          ? pageSize
          : Math.min(pageSize, Math.max(0, total - received));
      if (items.length >= expected) break;
      if (attempt + 1 < attemptsPerPage) await sleep(300 * 3 ** attempt);
    }
    received += items.length;
    for (const item of items) {
      const node = normalizeNode(item);
      if (!node) continue;
      const checked = Date.parse(item.last_check || "");
      const key = aggregateIdentity(node);
      const previous = nodes.get(key);
      const checkedTime = Number.isFinite(checked) ? checked : 0;
      if (!previous || checkedTime > previous.checkedTime)
        nodes.set(key, { node, checkedTime });
      if (Number.isFinite(checked)) newestCheck = Math.max(newestCheck, checked);
    }
    if (items.length < pageSize) break;
    if (total !== undefined && received >= total) break;
    if (page >= maxPages)
      throw new Error(`Qilan pagination exceeded ${maxPages} pages`);
    page += 1;
    await sleep(300);
  }
  // A truncated round is not a smaller panel: publishing it would mark every
  // missing connection as departed and burn probe budget re-checking them.
  if (total !== undefined && total - received > missingTolerance(total))
    throw new Error(
      `Qilan returned an incomplete node page set (${received}/${total}); previous snapshot retained`,
    );
  if (!nodes.size) throw new Error("Qilan returned no usable nodes");
  return {
    checkedAt: newestCheck
      ? new Date(newestCheck).toISOString()
      : new Date().toISOString(),
    speedTest: false,
    mediaCheck: false,
    nodes: [...nodes.values()].map(({ node }) => node),
  };
}

export async function loadQilanSession({ session, sessionFile } = {}) {
  if (session?.trim()) return session.trim();
  if (!sessionFile)
    throw new Error("Configure QILAN_SESSION or QILAN_SESSION_FILE");
  try {
    const value = (await readFile(sessionFile, "utf8")).trim();
    if (value) return value;
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  throw new Error("Qilan session file is missing or empty");
}

export function startQilanSource({
  store,
  session,
  sessionFile,
  userAgent,
  intervalMs = 15 * 60_000,
  fetchImpl,
  feishuWebhook,
  // Upstream can also understate `total` itself (throttled sessions once
  // reported 50 members for hours). What matters is not absolute size but the
  // step: a real panel does not shed 40% of its members between two rounds.
  minMemberRetention = 0.6,
  notify = createFeishuNotifier({ webhook: feishuWebhook }),
  onError = (error) => console.error(`Qilan source refresh failed: ${error.message}`),
} = {}) {
  let closed = false;
  let pending;
  let sessionBroken = false;
  let lastCount = 0;
  let panelShrunken = false;
  const refresh = async () => {
    if (closed || pending) return pending;
    pending = (async () => {
      const cookie = await loadQilanSession({ session, sessionFile });
      const snapshot = await fetchQilanSnapshot({
        session: cookie,
        userAgent,
        fetchImpl,
      });
      if (lastCount && snapshot.nodes.length < lastCount * minMemberRetention)
        throw new Error(
          `Qilan member count collapsed from ${lastCount} to ${snapshot.nodes.length}; round discarded`,
        );
      await store.syncSnapshot(snapshot);
      lastCount = snapshot.nodes.length;
      if (sessionBroken) {
        sessionBroken = false;
        await notify(
          `✅ Qilan 登录状态已恢复，节点快照同步继续。\n时间: ${new Date().toISOString()}`,
        );
      }
      if (panelShrunken) {
        panelShrunken = false;
        await notify(
          `✅ Qilan 抓取已恢复正常，本轮入库 ${snapshot.nodes.length} 个节点。\n时间: ${new Date().toISOString()}`,
        );
      }
    })()
      .catch(async (error) => {
        onError(error);
        // Session failures degrade silently (HTTP 403 or stripped share
        // links), so escalate them to the Feishu bot. Edge-triggered: one
        // alert per outage, one recovery notice when sync resumes.
        if (/session|403/i.test(String(error.message))) {
          const wasBroken = sessionBroken;
          sessionBroken = true;
          if (!wasBroken)
            await notify(
              `⚠️ Qilan 登录状态失效，节点快照同步已暂停。\n原因: ${error.message}\n时间: ${new Date().toISOString()}\n请更新 QILAN_SESSION（session.txt）后等待自动恢复。`,
            );
        } else if (/member count collapsed/.test(String(error.message))) {
          // Edge-triggered like session alerts, but independent: a throttled
          // upstream recovers on its own and needs no credential rotation.
          const wasShrunken = panelShrunken;
          panelShrunken = true;
          if (!wasShrunken)
            await notify(
              `⚠️ Qilan 本轮成员数骤降，已判定为抓取降级并丢弃该轮（保留上一快照）。\n原因: ${error.message}\n时间: ${new Date().toISOString()}`,
            );
        }
      })
      .finally(() => {
        pending = undefined;
      });
    return pending;
  };
  const timer = setInterval(refresh, intervalMs);
  timer.unref();
  void refresh();
  return {
    refresh,
    async close() {
      closed = true;
      clearInterval(timer);
      await pending;
    },
  };
}
