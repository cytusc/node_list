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
    timeout = 15000,
  },
  callback,
) {
  signal.throwIfAborted();
  if (!proxyUrl) throw new Error("IPPure 缺少隔离代理入口");
  const context = await createContext({
    proxy: { server: proxyUrl },
    timeout,
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

function exitProbeCode(error) {
  if (error.exitProbeCode) return error.exitProbeCode;
  const code = error.cause?.code || error.code;
  if (error.name === "TimeoutError" || /TIMEOUT/.test(code || ""))
    return "TIMEOUT";
  if (code === "ECONNRESET") return "CONNECTION_RESET";
  if (["ENOTFOUND", "EAI_AGAIN"].includes(code)) return "DNS_ERROR";
  if (/CERT|TLS|SSL/.test(code || "")) return "TLS_ERROR";
  // Transport messages can include proxy credentials. Persist categories only.
  return "NETWORK_ERROR";
}

// Cloudflare echoes the viewer's exit country as a two-letter code. Used to
// reconcile source-claimed regions (airport names frequently lie) with the
// actual geolocation of the verified exit IP.
function traceCountry(body) {
  return body.match(/^loc=([A-Za-z]{2})\s*$/m)?.[1]?.toUpperCase() || undefined;
}

// Racing four independent echo services halves the worst-case probe budget:
// one timeout window covers a whole batch instead of eight sequential dials.
// Each attempt keeps its own dispatcher, so a failed CONNECT/TLS session
// never poisons its batch neighbours.
const EXIT_PROBE_BATCH = 4;

const EXIT_PROBE_SERVICES = [
  ["https://api-ipv4.ip.sb/ip", "IP.SB"],
  ["https://checkip.amazonaws.com/", "AWS CheckIP"],
  ["https://api.ipify.org", "ipify"],
  ["https://ifconfig.me/ip", "ifconfig.me"],
  ["https://ipv4.icanhazip.com/", "icanhazip"],
  ["https://www.cloudflare.com/cdn-cgi/trace", "Cloudflare", "trace"],
  ["https://api.ip.sb/ip", "IP.SB"],
  ["https://icanhazip.com/", "icanhazip"],
];

export async function resolveExitIPInfo(options) {
  options.signal?.throwIfAborted();
  if (!options.proxyUrl) throw new Error("IPPure 缺少隔离代理入口");
  // A remembered endpoint for this node goes first: echo reachability is a
  // property of the exit, so the previous winner usually wins again.
  const preferred = new Set(options.preferredEndpoints || []);
  const services = [
    ...EXIT_PROBE_SERVICES.filter(([url]) => preferred.has(url)),
    ...EXIT_PROBE_SERVICES.filter(([url]) => !preferred.has(url)),
  ];
  let ipv6;
  let failures = 0;
  let deadStreak = 0;
  // Only unambiguous transport failures count towards the early abort;
  // NETWORK_ERROR is a catch-all and HTTP/TLS/DNS errors can be endpoint
  // specific while the tunnel itself is still alive.
  const transportDead = ["TIMEOUT", "CONNECTION_RESET"];
  const probeOne = async ([url, provider, format]) => {
    const started = Date.now();
    try {
      const outcome = await withHTTP(
        { ...options, timeout: options.timeoutMs ?? 5000 },
        async (context, signal) => {
          const response = await context.get(url);
          try {
            signal.throwIfAborted();
            if (!response.ok()) {
              const error = new Error("出口查询 HTTP 错误");
              error.exitProbeCode = `HTTP_${response.status()}`;
              throw error;
            }
            const body = (await response.text()).trim();
            const ip =
              format === "trace"
                ? body
                    .split(/\r?\n/)
                    .find((line) => line.startsWith("ip="))
                    ?.slice(3)
                    .trim()
                : body;
            if (!isIP(ip || "")) {
              const error = new Error("出口查询未返回有效 IP");
              error.exitProbeCode = "INVALID_IP";
              throw error;
            }
            const outcome = {
              ip,
              provider,
              addressFamily: isIP(ip),
              endpoint: url,
            };
            if (format === "trace") {
              outcome.country = traceCountry(body);
            } else if (outcome.addressFamily === 4) {
              // One extra request over the already-open tunnel buys the
              // exit country; a failure here never fails the probe itself.
              // IPv6 outcomes skip it: IPPure rejects IPv6 anyway, and the
              // lookup would be wasted whenever a later endpoint yields IPv4.
              try {
                const trace = await context.get(
                  "https://www.cloudflare.com/cdn-cgi/trace",
                );
                try {
                  signal.throwIfAborted();
                  if (trace.ok())
                    outcome.country = traceCountry(
                      (await trace.text()).trim(),
                    );
                } finally {
                  await trace.dispose();
                }
              } catch {
                /* best effort */
              }
            }
            return outcome;
          } finally {
            await response.dispose();
          }
        },
      );
      return { url, provider, started, outcome };
    } catch (error) {
      return { url, provider, started, error };
    }
  };
  for (let offset = 0; offset < services.length; offset += EXIT_PROBE_BATCH) {
    if (offset) options.signal?.throwIfAborted();
    const batch = services
      .slice(offset, offset + EXIT_PROBE_BATCH)
      .map((service) => probeOne(service));
    // Report in endpoint order so diagnostics stay deterministic even
    // though the dials race.
    const attempts = await Promise.all(batch);
    options.signal?.throwIfAborted();
    let ipv4Outcome;
    let transportFailures = 0;
    let endpointSpecific = false;
    for (const { url, provider, started, outcome, error } of attempts) {
      if (error) {
        const code = exitProbeCode(error);
        options.onProbeAttempt?.({
          provider,
          endpoint: url,
          round: 1,
          elapsedMs: Date.now() - started,
          outcome: "error",
          code,
        });
        if (transportDead.includes(code)) transportFailures++;
        else endpointSpecific = true;
        continue;
      }
      options.onProbeAttempt?.({
        provider,
        endpoint: url,
        round: 1,
        elapsedMs: Date.now() - started,
        outcome: outcome.addressFamily === 4 ? "ipv4" : "ipv6",
      });
      if (outcome.addressFamily === 4) ipv4Outcome ??= outcome;
      else ipv6 ??= outcome;
    }
    if (ipv4Outcome) return ipv4Outcome;
    failures += attempts.filter((attempt) => attempt.error).length;
    // Endpoint-specific failures prove the tunnel answers per destination;
    // only uniform transport-level failure across a batch marks it dead.
    deadStreak = endpointSpecific
      ? 0
      : deadStreak + transportFailures;
    if (deadStreak >= 2) break;
  }
  if (ipv6) return ipv6;
  const failure =
    deadStreak >= 2
      ? new Error("节点出口探测连续多次传输级失败，已提前中止")
      : new Error(
          `出口 IP 探测失败：${failures} 个查询入口均未取得有效 IP`,
        );
  failure.exitIPFailure = true;
  throw failure;
}

// Preserve the IPv4-only public helper used by maintenance scripts.
export async function resolveExitIP(options) {
  const data = await resolveExitIPInfo(options);
  if (data.addressFamily !== 4)
    throw new Error(`${data.provider} 返回 IPv6；IPPure 风险评分接口需要 IPv4`);
  return data.ip;
}

// Best-effort country lookup for nodes whose exit IP is already verified but
// were probed before country collection existed. Rides one Cloudflare trace
// request through the node's own transport.
export async function resolveExitCountry(options) {
  return withHTTP(
    { ...options, timeout: options.timeoutMs ?? 5000 },
    async (context, signal) => {
      const response = await context.get(
        "https://www.cloudflare.com/cdn-cgi/trace",
      );
      try {
        signal.throwIfAborted();
        if (!response.ok())
          throw new Error(`出口国家查询 HTTP 错误 (${response.status()})`);
        const country = traceCountry((await response.text()).trim());
        if (!country) throw new Error("出口国家查询未返回有效地区码");
        return country;
      } finally {
        await response.dispose();
      }
    },
  );
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
  // The basic endpoint follows IPInfo's shape and usually carries an ISO
  // country code; treat it as optional so older deployments stay compatible.
  const apiCountry =
    typeof basic.country === "string" && /^[A-Za-z]{2}$/.test(basic.country)
      ? basic.country.toUpperCase()
      : undefined;
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
    ...(apiCountry ? { country: apiCountry } : {}),
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
