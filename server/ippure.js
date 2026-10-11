import { readFile } from "node:fs/promises";
import path from "node:path";
import { isIP } from "node:net";
import { parse, stringify } from "yaml";
import { chromium } from "playwright-core";

// Runs inside the browser. Do not depend on Vue's generated data-v attributes.
export function extractIPPure() {
  const table = document.querySelector(".iptable-container");
  if (!table) throw new Error("IPPure 信息表未加载");
  const text = (element) =>
    element?.textContent?.trim().replace(/\s+/g, " ") || "";
  const value = (label) => {
    const key = [...table.querySelectorAll(".info-key")].find(
      (e) => text(e) === label,
    );
    return text(key?.parentElement?.querySelector(".info-value"));
  };
  const ratio = text(table.querySelector(".botclass-value"));
  const human = ratio.match(/human\s*([\d.]+)%/i);
  const bot = ratio.match(/bot\s*([\d.]+)%/i);
  const score = text(table.querySelector(".colormap-indicator-value")).match(
    /^(\d+(?:\.\d+)?)%\s*(.+)$/,
  );
  const ip = value("IP");
  const asnValue = value("ASN").match(/^(AS\d+)\s*[-–—]\s*(.+)$/i);
  if (!asnValue) throw new Error("IPPure ASN 组织信息不完整");
  const source = value("IP来源");
  const attributes = [...table.querySelectorAll(".ip-attr .ip-tag")]
    .map(text)
    .filter(Boolean);
  if (!ip || !source || !attributes.length || !score)
    throw new Error("IPPure 返回信息不完整");
  const percentage = (v) => {
    const n = Number(v);
    if (!Number.isFinite(n) || n < 0 || n > 100)
      throw new Error("IPPure 百分比无效");
    return n;
  };
  if (!!human !== !!bot) throw new Error("IPPure 人机流量比不完整");
  return {
    ip,
    asn: asnValue[1].toUpperCase(),
    asnOrganization: asnValue[2],
    source,
    attributes,
    humanPercent: human ? percentage(human[1]) : null,
    botPercent: bot ? percentage(bot[1]) : null,
    score: percentage(score[1]),
    label: score[2],
  };
}

async function withProxyBrowser(
  {
    controller,
    proxyUrl,
    secret,
    executablePath,
    proxy,
    signal,
    launchBrowser = chromium.launch,
  },
  callback,
) {
  const config = {
    "mixed-port": Number(new URL(proxyUrl).port),
    "allow-lan": true,
    "bind-address": "*",
    mode: "rule",
    "external-controller": `0.0.0.0:${new URL(controller).port || (new URL(controller).protocol === "https:" ? 443 : 80)}`,
    secret: secret || "",
    proxies: [{ ...proxy, name: "ippure-node" }],
    rules: ["MATCH,ippure-node"],
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
  const browser = await launchBrowser({
    executablePath,
    args: [
      "--no-sandbox",
      "--disable-dev-shm-usage",
      "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
    ],
    proxy: { server: proxyUrl },
    timeout: 15000,
  });
  const abort = () => void browser.close();
  signal.addEventListener("abort", abort, { once: true });
  try {
    signal.throwIfAborted();
    return await callback(browser);
  } finally {
    signal.removeEventListener("abort", abort);
    await browser.close();
  }
}

export async function resolveExitIP(options) {
  return withProxyBrowser(options, async (browser) => {
    const page = await browser.newPage();
    // The request sees the target node's egress; never resolve its hostname
    // locally or fall back to a direct connection.
    options.signal.throwIfAborted();
    const response = await page.goto("https://api.ip.sb/ip", {
      waitUntil: "domcontentloaded",
      timeout: 15000,
    });
    if (!response?.ok())
      throw new Error(`IP.SB 访问失败 (HTTP ${response?.status()})`);
    const ip = (await page.locator("body").innerText()).trim();
    if (!isIP(ip)) throw new Error("IP.SB 未返回有效的出口 IP");
    return ip;
  });
}

export async function queryIPPure(options) {
  const { target } = options;
  if (target && !isIP(target)) throw new Error("IPPure 查询目标必须是出口 IP");
  return withProxyBrowser(options, async (browser) => {
    const page = await browser.newPage({ locale: "zh-CN" });
    page.setDefaultTimeout(45000);
    const url = new URL("https://ippure.com/");
    if (target) url.searchParams.set("ip", target);
    const response = await page.goto(url.href, {
      waitUntil: "domcontentloaded",
      timeout: 45000,
    });
    if (!response?.ok())
      throw new Error(`IPPure 访问失败 (HTTP ${response?.status()})`);
    const table = page.locator(".iptable-container").first();
    await table.locator(".colormap-indicator-value").waitFor();
    const asn = table.locator(".info-key").filter({ hasText: /^ASN$/ });
    await asn.locator("..").locator("button").first().click();
    await page.waitForFunction(
      () => {
        const table = document.querySelector(".iptable-container");
        return (
          table?.querySelector(".botclass-value") ||
          /人机流量比[\s\S]*(统计数据不足|无记录|暂无|N\/A)/.test(
            table?.textContent || "",
          )
        );
      },
      undefined,
      { timeout: 15000 },
    );
    const data = await page.evaluate(extractIPPure);
    const normalizeIP = (ip) =>
      isIP(ip) === 6 ? new URL(`http://[${ip}]/`).hostname : ip;
    if (target && normalizeIP(target) !== normalizeIP(data.ip))
      throw new Error("IPPure 返回 IP 与查询出口 IP 不一致");
    return data;
  });
}

export function startIPPureCollector({
  store,
  subDir,
  controller,
  proxyUrl,
  secret,
  executablePath,
  query = queryIPPure,
  resolveIP = resolveExitIP,
  intervalMs = 30000,
}) {
  const abort = new AbortController();
  let pending;
  let lastError = null;
  let rotation = 0;
  async function collect() {
    const snapshot = await store.get();
    if (!snapshot?.nodes.length) return;
    const document = parse(
      await readFile(path.join(subDir, "output", "all.yaml"), "utf8"),
    );
    if (!Array.isArray(document?.proxies))
      throw new Error("Clash 订阅缺少 proxies");
    const matchProxy = (node) => {
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
    const working = snapshot.nodes
      .filter((node) => node.ippure?.status === "success")
      .map(matchProxy)
      .filter(Boolean);
    const unavailable = new Set();
    for (const [index, node] of snapshot.nodes.entries()) {
      if (abort.signal.aborted) return;
      if ((await store.get())?.id !== snapshot.id) return;
      const old = node.ippure;
      const currentExitIP = old?.exitIPSource === "ip.sb";
      const currentExitMode = old?.exitIPMode === "ip.sb";
      const backfill =
        old?.status === "success" && currentExitIP && !old.asnOrganization;
      if (old?.status === "success" && currentExitIP && !backfill) continue;
      // Old failures used the target as transport. Give the new query mode
      // its own retry budget, including nodes that exhausted the old budget.
      const attempts =
        currentExitMode && old?.queryMode === "specified-exit"
          ? old.attempts || 0
          : 0;
      if ((backfill ? old.organizationAttempts || 0 : attempts) >= 3) continue;
      const lastAttempt = backfill
        ? old.organizationCheckedAt
        : currentExitMode && old?.queryMode === "specified-exit"
          ? old.checkedAt
          : undefined;
      if (lastAttempt && Date.now() - Date.parse(lastAttempt) < 30 * 60 * 1000)
        continue;
      // A rewritten subscription may belong to a different detection round.
      const ownProxy = matchProxy(node);
      if (!ownProxy) continue;
      const result = {
        checkedAt: new Date().toISOString(),
        attempts: attempts + 1,
        queryMode: "specified-exit",
        exitIPMode: "ip.sb",
      };
      try {
        const options = {
          controller,
          proxyUrl,
          secret,
          executablePath,
          signal: abort.signal,
        };
        const target =
          currentExitIP && typeof old?.ip === "string" && isIP(old.ip)
            ? old.ip
            : await resolveIP({ ...options, proxy: ownProxy });
        if (typeof target !== "string" || !isIP(target))
          throw new Error("IPPure 无法获取有效的节点出口 IP");
        result.ip = target;
        result.exitIPSource = "ip.sb";
        result.queryTarget = target;
        const available = working.filter((proxy) => !unavailable.has(proxy));
        // Bootstrap the pool through a node itself when no usable donor exists.
        const candidates = available.length
          ? Array.from(
              { length: Math.min(3, available.length) },
              (_, offset) => available[(rotation + offset) % available.length],
            )
          : [ownProxy];
        rotation++;
        let failure;
        for (const proxy of candidates) {
          try {
            const data = await query({
              controller,
              proxyUrl,
              secret,
              executablePath,
              proxy,
              target,
              signal: abort.signal,
            });
            Object.assign(result, data, { status: "success" });
            if (!working.includes(proxy)) working.push(proxy);
            break;
          } catch (error) {
            if (abort.signal.aborted) return;
            unavailable.add(proxy);
            failure = error;
          }
        }
        if (result.status !== "success") throw failure;
      } catch (error) {
        if (abort.signal.aborted) return;
        if (backfill) {
          // Keep previously collected metrics if the organization refresh fails.
          await store.saveIPPure(snapshot.id, index, {
            ...old,
            organizationAttempts: (old.organizationAttempts || 0) + 1,
            organizationCheckedAt: result.checkedAt,
          });
          continue;
        }
        result.status = "error";
        // Never persist proxy credentials or controller response bodies.
        result.error = /IPPure|IP\.SB|代理配置/.test(error.message)
          ? error.message.slice(0, 160)
          : "节点访问或页面提取失败，请稍后重试";
      }
      if ((await store.get())?.id !== snapshot.id) return;
      await store.saveIPPure(snapshot.id, index, result);
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
    status: () => ({ enabled: true, running: !!pending, error: lastError }),
    async close() {
      clearInterval(timer);
      abort.abort();
      await pending;
    },
  };
}
