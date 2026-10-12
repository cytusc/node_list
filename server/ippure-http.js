import { createHmac } from "node:crypto";
import { isIP } from "node:net";
import { setTimeout as delay } from "node:timers/promises";
import { fetch as proxyFetch, ProxyAgent } from "undici";
import { stringify } from "yaml";

// IPPure's signing session must stay on one proxy connection. A general pool
// may send the challenge and signed retry over different tunnels.
export function createHTTPContext({ proxy, timeout, extraHTTPHeaders }) {
  const dispatcher = new ProxyAgent({
    uri: proxy.server,
    connections: 1,
    pipelining: 1,
  });
  const abort = new AbortController();
  return {
    async get(url, { headers = {} } = {}) {
      const response = await proxyFetch(url, {
        dispatcher,
        headers: { ...extraHTTPHeaders, ...headers },
        redirect: "error",
        signal: AbortSignal.any([abort.signal, AbortSignal.timeout(timeout)]),
      });
      return {
        headers: () => Object.fromEntries(response.headers),
        status: () => response.status,
        ok: () => response.ok,
        text: () => response.text(),
        json: () => response.json(),
        // Drain challenges before retrying so the tunnel remains reusable.
        dispose: async () => {
          if (!response.bodyUsed) await response.arrayBuffer();
        },
      };
    },
    async dispose() {
      abort.abort();
      await dispatcher.destroy();
    },
  };
}

async function withHTTP(
  {
    proxyUrl,
    signal = new AbortController().signal,
    createContext = createHTTPContext,
  },
  callback,
) {
  signal.throwIfAborted();
  if (!proxyUrl) throw new Error("IPPure 缺少隔离代理入口");
  const context = await createContext({
    proxy: { server: proxyUrl },
    timeout: 15000,
    extraHTTPHeaders: {
      Referer: "https://ippure.com/",
      Origin: "https://ippure.com",
    },
  });
  const abort = () => void context.dispose().catch(() => {});
  signal.addEventListener("abort", abort, { once: true });
  try {
    signal.throwIfAborted();
    return await callback(context, signal);
  } finally {
    signal.removeEventListener("abort", abort);
    await context.dispose();
  }
}

export async function configureProxyBatch({
  controller,
  proxyUrl,
  secret,
  proxies,
  signal,
}) {
  const base = new URL(proxyUrl);
  const port = Number(base.port || (base.protocol === "https:" ? 443 : 80));
  if (port < 1 || port + proxies.length > 65535)
    throw new Error("代理配置端口范围无效");
  const urls = new Map(
    proxies.map((proxy, i) => {
      const url = new URL(base);
      url.port = String(port + i + 1);
      return [proxy, url.href];
    }),
  );
  const config = {
    // Keep the original entry fail-closed. Reusing its port for a listener
    // during Mihomo's reload can fail to bind before that entry is closed.
    "mixed-port": port,
    "allow-lan": true,
    "bind-address": "*",
    mode: "rule",
    "external-controller": `0.0.0.0:${new URL(controller).port || 9090}`,
    secret: secret || "",
    proxies: proxies.map((proxy, i) => ({ ...proxy, name: `ippure-${i}` })),
    listeners: proxies.map((_, i) => ({
      name: `ippure-${i}`,
      type: "mixed",
      port: port + i + 1,
      listen: "0.0.0.0",
      proxy: `ippure-${i}`,
    })),
    rules: ["MATCH,REJECT"],
  };
  const response = await fetch(
    `${controller.replace(/\/$/, "")}/configs?force=true`,
    {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(secret ? { Authorization: `Bearer ${secret}` } : {}),
      },
      body: JSON.stringify({ payload: stringify(config) }),
      signal: AbortSignal.any([signal, AbortSignal.timeout(15000)]),
    },
  );
  if (!response.ok) throw new Error(`代理配置失败 (HTTP ${response.status})`);
  return urls;
}

export async function resolveExitIP(options) {
  return withHTTP(options, async (context, signal) => {
    let failure;
    // IPPure's risk endpoint accepts IPv4 only. Both probes must use the node.
    for (const url of ["https://api-ipv4.ip.sb/ip", "https://api.ip.sb/ip"]) {
      try {
        const response = await context.get(url);
        try {
          signal.throwIfAborted();
          if (!response.ok())
            throw new Error(`IP.SB 访问失败 (HTTP ${response.status()})`);
          const ip = (await response.text()).trim();
          if (!isIP(ip)) throw new Error("IP.SB 未返回有效的出口 IP");
          if (isIP(ip) !== 4)
            throw new Error(
              "IP.SB 未返回 IPv4 出口，IPPure 风险评分接口暂不支持 IPv6",
            );
          return ip;
        } finally {
          await response.dispose();
        }
      } catch (error) {
        signal.throwIfAborted();
        failure = error;
      }
    }
    throw failure;
  });
}

export function createSignedClient(context, { origin, signal, wait = delay }) {
  let key;
  let timeOffset = 0;
  return async (path) => {
    const url = new URL(path, origin).href;
    let challenges = 0;
    let retries = 0;
    while (true) {
      signal.throwIfAborted();
      const headers = {};
      if (key) {
        const time = Date.now() + timeOffset;
        const signature = createHmac("sha256", key)
          .update(["GET", url, "", time].join("-"))
          .digest("hex");
        headers["x-k"] = key;
        headers["x-t"] = `${time}-${signature}`;
      }
      const response = await context.get(url, { headers });
      signal.throwIfAborted();
      const issuedKey = response.headers()["x-k"];
      if (issuedKey) {
        const serverTime = Number(response.headers()["x-t"]);
        await response.dispose();
        if (++challenges > 3 || !Number.isFinite(serverTime))
          throw new Error("IPPure 接口签名验证失败");
        key = issuedKey;
        timeOffset = serverTime - Date.now();
        continue;
      }
      const status = response.status();
      if ((status === 429 || status >= 500) && retries < 2) {
        const retryAfter = Number(response.headers()["retry-after"]);
        await response.dispose();
        await wait(
          Math.min(
            5000,
            Math.max(1000 * 2 ** retries++, retryAfter * 1000 || 0),
          ),
          undefined,
          { signal },
        );
        continue;
      }
      if (!response.ok()) {
        await response.dispose();
        throw new Error(`IPPure 接口访问失败 (HTTP ${status})`);
      }
      let result;
      try {
        result = await response.json();
      } catch {
        throw new Error("IPPure 接口未返回有效 JSON");
      } finally {
        await response.dispose();
      }
      if (result?.ok !== true || !("data" in result))
        throw new Error("IPPure 接口返回失败");
      return result.data;
    }
  };
}

function percentage(value) {
  if (
    typeof value !== "number" ||
    !Number.isFinite(value) ||
    value < 0 ||
    value > 100
  )
    throw new Error("IPPure 百分比无效");
  return value;
}

function botStatistics(value) {
  if (value == null || (value.human === 0 && value.bot === 0)) return null;
  return { human: percentage(value.human), bot: percentage(value.bot) };
}

export function normalizeIPPure(target, basic, risk, bot) {
  const normalizeIP = (ip) =>
    isIP(ip) === 6 ? new URL(`http://[${ip}]/`).hostname : ip;
  if (
    !basic ||
    !isIP(basic.ip || "") ||
    normalizeIP(basic.ip) !== normalizeIP(target)
  )
    throw new Error("IPPure 返回 IP 与查询出口 IP 不一致");
  if (
    !Number.isInteger(basic.asn?.number) ||
    basic.asn.number < 1 ||
    typeof basic.asn.organization !== "string" ||
    !basic.asn.organization.trim() ||
    typeof basic.traits?.is_broadcast !== "boolean" ||
    !basic.asn.type
  )
    throw new Error("IPPure 返回信息不完整");
  const score = percentage(risk?.risk_score);
  const boundaries = [15, 25, 40, 50, 70, 100];
  const labels = [
    "极度纯净",
    "纯净",
    "中性",
    "轻度风险",
    "中度风险",
    "极度风险",
  ];
  const types = {
    isp: "住宅IP",
    hosting: "机房IP",
    business: "机房IP",
    enterprise: "商业宽带",
    education: "教育IP",
    government: "政府IP",
  };
  const statistics = botStatistics(bot);
  const attributes = [types[basic.asn.type] || "机房IP"];
  if (basic.asn.type === "business" && statistics?.human > 50)
    attributes.push("商业用途");
  return {
    ip: basic.ip,
    asn: `AS${basic.asn.number}`,
    asnOrganization: basic.asn.organization.trim(),
    source: basic.traits.is_broadcast ? "广播IP" : "原生IP",
    attributes,
    humanPercent: statistics ? Number(statistics.human.toFixed(2)) : null,
    botPercent: statistics ? Number(statistics.bot.toFixed(2)) : null,
    score,
    label: labels[boundaries.findIndex((end) => score <= end)],
  };
}

export async function queryIPPure(options) {
  const {
    target,
    apiOrigin = "https://api.123169.xyz",
    asnCache = new Map(),
  } = options;
  if (!isIP(target || "")) throw new Error("IPPure 查询目标必须是出口 IP");
  if (isIP(target) === 6)
    throw new Error("节点仅获取到 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6");
  return withHTTP(options, async (context, signal) => {
    const get = createSignedClient(context, { origin: apiOrigin, signal });
    // Keep requests in one signing session sequential: challenges can rotate its key.
    const basic = await get(`/api/info/ip-basic/${target}`);
    const risk = await get(`/api/info/ip-risk/${target}`);
    const asn = basic?.asn?.number;
    if (!Number.isInteger(asn) || asn < 1)
      throw new Error("IPPure ASN 信息不完整");
    let bot = asnCache.get(asn);
    if (!bot) {
      bot = get(`/api/info/asn/botclass/${asn}`).then(botStatistics);
      asnCache.set(asn, bot);
      bot.catch(() => {
        if (asnCache.get(asn) === bot) asnCache.delete(asn);
      });
    }
    const data = normalizeIPPure(target, basic, risk, await bot);
    signal.throwIfAborted();
    return data;
  });
}
