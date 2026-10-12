import { readFile } from "node:fs/promises";

const baseUrl = "https://www.qilan.de";
const pageSize = 200;

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
  const nodes = [];
  let page = 1;
  let total;
  let newestCheck = 0;
  while (true) {
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
    for (const item of payload.data) {
      const node = normalizeNode(item);
      if (!node) continue;
      nodes.push(node);
      const checked = Date.parse(item.last_check || "");
      if (Number.isFinite(checked)) newestCheck = Math.max(newestCheck, checked);
    }
    if (payload.data.length < pageSize) break;
    if (total !== undefined && page * pageSize >= total) break;
    page += 1;
    await sleep(300);
  }
  if (!nodes.length) throw new Error("Qilan returned no usable nodes");
  return {
    checkedAt: newestCheck
      ? new Date(newestCheck).toISOString()
      : new Date().toISOString(),
    speedTest: false,
    mediaCheck: false,
    nodes,
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
  onError = (error) => console.error(`Qilan source refresh failed: ${error.message}`),
} = {}) {
  let closed = false;
  let pending;
  const refresh = async () => {
    if (closed || pending) return pending;
    pending = (async () => {
      const cookie = await loadQilanSession({ session, sessionFile });
      const snapshot = await fetchQilanSnapshot({
        session: cookie,
        userAgent,
        fetchImpl,
      });
      await store.syncSnapshot(snapshot);
    })()
      .catch(onError)
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
