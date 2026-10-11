import { readFile } from "node:fs/promises";
import path from "node:path";
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
    source,
    attributes,
    humanPercent: human ? percentage(human[1]) : null,
    botPercent: bot ? percentage(bot[1]) : null,
    score: percentage(score[1]),
    label: score[2],
  };
}

export async function queryIPPure({
  controller,
  proxyUrl,
  secret,
  executablePath,
  proxy,
  signal,
}) {
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
  const browser = await chromium.launch({
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
    const page = await browser.newPage({ locale: "zh-CN" });
    page.setDefaultTimeout(45000);
    const response = await page.goto("https://ippure.com/", {
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
    return await page.evaluate(extractIPPure);
  } finally {
    signal.removeEventListener("abort", abort);
    await browser.close();
  }
}

export function startIPPureCollector({
  store,
  subDir,
  controller,
  proxyUrl,
  secret,
  executablePath,
  query = queryIPPure,
  intervalMs = 30000,
}) {
  const abort = new AbortController();
  let pending;
  let lastError = null;
  async function collect() {
    const snapshot = await store.get();
    if (!snapshot?.nodes.length) return;
    const document = parse(
      await readFile(path.join(subDir, "output", "all.yaml"), "utf8"),
    );
    if (!Array.isArray(document?.proxies))
      throw new Error("Clash 订阅缺少 proxies");
    for (const [index, node] of snapshot.nodes.entries()) {
      if (abort.signal.aborted) return;
      if ((await store.get())?.id !== snapshot.id) return;
      const old = node.ippure;
      if (old?.status === "success" || old?.attempts >= 3) continue;
      if (old && Date.now() - Date.parse(old.checkedAt) < 30 * 60 * 1000)
        continue;
      const matches = document.proxies.filter(
        (p) =>
          p.name === node.name &&
          p.type === node.type &&
          p.server === node.server &&
          String(p.port) === String(node.port),
      );
      // A rewritten subscription may belong to a different detection round.
      if (matches.length !== 1) continue;
      const result = {
        checkedAt: new Date().toISOString(),
        attempts: (old?.attempts || 0) + 1,
      };
      try {
        Object.assign(
          result,
          await query({
            controller,
            proxyUrl,
            secret,
            executablePath,
            proxy: matches[0],
            signal: abort.signal,
          }),
          { status: "success" },
        );
      } catch (error) {
        if (abort.signal.aborted) return;
        result.status = "error";
        // Never persist proxy credentials or controller response bodies.
        result.error = /IPPure|代理配置/.test(error.message)
          ? error.message.slice(0, 160)
          : "节点访问或页面提取失败，请稍后重试";
      }
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
