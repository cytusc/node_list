import { test } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { createServer } from "node:http";
import { connect } from "node:net";
import { request } from "playwright-core";
import { parse } from "yaml";
import {
  configureProxyBatch,
  createSignedClient,
  normalizeIPPure,
  queryIPPure,
  resolveExitIP,
} from "./ippure-http.js";

const signal = () => new AbortController().signal;
const basic = (ip = "23.22.22.12") => ({
  ip,
  asn: { number: 14618, organization: "Amazon", type: "hosting" },
  traits: { is_broadcast: false },
});

test("production HTTP transport keeps challenge, signed retry and API queries on one proxy tunnel", async (t) => {
  const connections = new Set();
  let calls = 0;
  const backend = createServer((req, res) => {
    calls++;
    connections.add(req.socket.remotePort);
    const key = `session-${req.socket.remotePort}`;
    const time = req.headers["x-t"]?.split("-")[0];
    const url = `http://127.0.0.1:${backend.address().port}${req.url}`;
    const signed = `${time}-${createHmac("sha256", key).update(["GET", url, "", time].join("-")).digest("hex")}`;
    if (req.headers["x-k"] !== key || req.headers["x-t"] !== signed) {
      res.writeHead(200, { "x-k": key, "x-t": String(Date.now()) });
      res.end(JSON.stringify({ ok: false }));
      return;
    }
    assert.equal(req.headers.origin, "https://ippure.com");
    res.end(
      JSON.stringify({
        ok: true,
        data: req.url.includes("ip-basic")
          ? basic()
          : req.url.includes("ip-risk")
            ? { risk_score: 36 }
            : { human: 2, bot: 98 },
      }),
    );
  });
  await new Promise((r) => backend.listen(0, "127.0.0.1", r));
  t.after(() => backend.close());
  let tunnels = 0;
  const sockets = new Set();
  const proxy = createServer();
  proxy.on("connect", (req, client, head) => {
    tunnels++;
    assert.equal(req.url, `127.0.0.1:${backend.address().port}`);
    const upstream = connect(backend.address().port, "127.0.0.1", () => {
      client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
      if (head.length) upstream.write(head);
      upstream.pipe(client);
      client.pipe(upstream);
    });
    sockets.add(client);
    sockets.add(upstream);
    client.on("error", () => upstream.destroy());
    upstream.on("error", () => client.destroy());
  });
  await new Promise((r) => proxy.listen(0, "127.0.0.1", r));
  t.after(() => {
    for (const socket of sockets) socket.destroy();
    proxy.close();
  });
  const result = await queryIPPure({
    target: "23.22.22.12",
    proxyUrl: `http://127.0.0.1:${proxy.address().port}`,
    apiOrigin: `http://127.0.0.1:${backend.address().port}`,
  });
  assert.equal(result.score, 36);
  assert.equal(result.humanPercent, 2);
  assert.equal(calls, 4);
  assert.equal(tunnels, 1);
  assert.equal(connections.size, 1);
});

test("signed HTTP client negotiates a key and signs the exact URL using server time", async (t) => {
  let count = 0;
  const server = createServer((req, res) => {
    count++;
    if (!req.headers["x-k"]) {
      res.writeHead(200, {
        "x-k": "test-key",
        "x-t": String(Date.now() - 90000),
      });
      res.end(JSON.stringify({ ok: false }));
      return;
    }
    const signed = req.headers["x-t"];
    const time = signed.split("-")[0];
    assert.ok(Math.abs(Number(time) - (Date.now() - 90000)) < 2000);
    const url = `http://127.0.0.1:${server.address().port}${req.url}`;
    assert.equal(
      signed,
      `${time}-${createHmac("sha256", "test-key").update(["GET", url, "", time].join("-")).digest("hex")}`,
    );
    res.end(JSON.stringify({ ok: true, data: { risk_score: 36 } }));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const context = await request.newContext();
  t.after(() => context.dispose());
  const get = createSignedClient(context, {
    origin: `http://127.0.0.1:${server.address().port}`,
    signal: signal(),
  });
  assert.deepEqual(await get("/api/info/ip-risk/23.22.22.12"), {
    risk_score: 36,
  });
  assert.deepEqual(await get("/api/info/ip-risk/23.22.22.13"), {
    risk_score: 36,
  });
  assert.equal(count, 3);
});

const response = (data, status = 200, headers = {}) => ({
  headers: () => headers,
  status: () => status,
  ok: () => status >= 200 && status < 300,
  json: async () => data,
  text: async () => data,
  dispose: async () => {},
});

test("rate limits back off; HTTP 200 failures and repeated challenges are rejected", async () => {
  const waits = [];
  let calls = 0;
  const get = createSignedClient(
    {
      get: async () =>
        ++calls <= 2
          ? response(null, 429, { "retry-after": "100" })
          : response({ ok: true, data: 0 }),
    },
    {
      origin: "https://example.com",
      signal: signal(),
      wait: async (ms) => waits.push(ms),
    },
  );
  assert.equal(await get("/test"), 0);
  assert.deepEqual(waits, [5000, 5000]);
  await assert.rejects(
    createSignedClient(
      { get: async () => response({ ok: false }) },
      { origin: "https://example.com", signal: signal() },
    )("/test"),
    /返回失败/,
  );
  calls = 0;
  await assert.rejects(
    createSignedClient(
      {
        get: async () => {
          calls++;
          return response(null, 200, {
            "x-k": "key",
            "x-t": String(Date.now()),
          });
        },
      },
      { origin: "https://example.com", signal: signal() },
    )("/test"),
    /签名验证失败/,
  );
  assert.equal(calls, 4);
});

test("API values preserve IPPure labels, zero values, missing statistics and IPv6 identity", () => {
  for (const [score, label] of [
    [0, "极度纯净"],
    [15, "极度纯净"],
    [25, "纯净"],
    [36, "中性"],
    [40, "中性"],
    [50, "轻度风险"],
    [70, "中度风险"],
    [100, "极度风险"],
  ]) {
    const data = normalizeIPPure(
      "23.22.22.12",
      basic(),
      { risk_score: score },
      { human: 0, bot: 100 },
    );
    assert.equal(data.label, label);
    assert.equal(data.humanPercent, 0);
    assert.equal(data.source, "原生IP");
    assert.deepEqual(data.attributes, ["机房IP"]);
  }
  assert.equal(
    normalizeIPPure("23.22.22.12", basic(), { risk_score: 0 }, null)
      .humanPercent,
    null,
  );
  assert.equal(
    normalizeIPPure(
      "2001:db8::1",
      basic("2001:0db8:0:0:0:0:0:1"),
      { risk_score: 0 },
      null,
    ).score,
    0,
  );
  assert.throws(
    () => normalizeIPPure("23.22.22.13", basic(), { risk_score: 0 }, null),
    /不一致/,
  );
  assert.throws(
    () => normalizeIPPure("23.22.22.12", basic(), { risk_score: "0" }, null),
    /百分比/,
  );
  assert.throws(
    () =>
      normalizeIPPure("23.22.22.12", basic(), { risk_score: 0 }, { human: 20 }),
    /百分比/,
  );
});

test("queries use only HTTP contexts through assigned proxy and deduplicate ASN requests", async () => {
  const calls = [];
  const disposed = [];
  const asnCache = new Map();
  const createContext = async ({ proxy }) => ({
    get: async (url) => {
      calls.push([proxy.server, url]);
      const target = url.split("/").at(-1);
      return response({
        ok: true,
        data: url.includes("ip-basic")
          ? basic(target)
          : url.includes("ip-risk")
            ? { risk_score: 36 }
            : { human: 2.914466, bot: 97.085534 },
      });
    },
    dispose: async () => disposed.push(proxy.server),
  });
  const results = await Promise.all(
    [12, 13].map((i) =>
      queryIPPure({
        target: `23.22.22.${i}`,
        proxyUrl: `http://localhost:${7900 + i}`,
        createContext,
        asnCache,
        signal: signal(),
      }),
    ),
  );
  assert.equal(results[0].humanPercent, 2.91);
  assert.equal(calls.filter(([, url]) => url.includes("botclass")).length, 1);
  assert.equal(disposed.length, 2);
  assert.ok(
    calls.some(
      ([proxy, url]) => proxy.endsWith(":7913") && url.endsWith("23.22.22.13"),
    ),
  );
});

test("exit resolution never falls back to a direct request and abort disposes its context", async () => {
  let disposed = 0;
  const abort = new AbortController();
  let started;
  const ready = new Promise((r) => {
    started = r;
  });
  let rejectRequest;
  const pending = resolveExitIP({
    proxyUrl: "http://localhost:7890",
    signal: abort.signal,
    createContext: async ({ proxy }) => {
      assert.equal(proxy.server, "http://localhost:7890");
      return {
        get: async (url) => {
          assert.equal(url, "https://api-ipv4.ip.sb/ip");
          started();
          return new Promise((_, reject) => {
            rejectRequest = reject;
          });
        },
        dispose: async () => {
          disposed++;
          rejectRequest?.(new Error("cancelled"));
        },
      };
    },
  });
  await ready;
  abort.abort();
  await assert.rejects(pending);
  assert.ok(disposed > 0);
  await assert.rejects(resolveExitIP({ signal: signal() }), /缺少隔离代理/);
});

test("Mihomo configuration binds each independent listener to the corresponding node and rejects unmatched traffic", async (t) => {
  let config;
  const server = createServer(async (req, res) => {
    assert.equal(req.method, "PUT");
    let body = "";
    for await (const chunk of req) body += chunk;
    config = parse(JSON.parse(body).payload);
    res.writeHead(204).end();
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  t.after(() => server.close());
  const proxies = [0, 1, 2].map((i) => ({
    name: `node${i}`,
    type: "http",
    server: `node${i}.example`,
    port: 443,
    password: "private",
  }));
  const urls = await configureProxyBatch({
    controller: `http://127.0.0.1:${server.address().port}`,
    proxyUrl: "http://127.0.0.1:7890",
    proxies,
    signal: signal(),
  });
  assert.equal(new Set(urls.values()).size, 3);
  assert.equal(config["mixed-port"], 7890);
  assert.deepEqual(config.rules, ["MATCH,REJECT"]);
  for (const [i, listener] of config.listeners.entries()) {
    assert.equal(listener.port, 7891 + i);
    assert.equal(listener.proxy, config.proxies[i].name);
    assert.equal(config.proxies[i].server, proxies[i].server);
  }
});

test("exit discovery prefers IPv4 and only falls back through the same node", async () => {
  for (const first of ["192.0.2.20", "2001:db8::1", "failure"]) {
    const calls = [];
    const ip = await resolveExitIP({
      proxyUrl: "http://localhost:7891",
      createContext: ({ proxy }) => {
        assert.equal(proxy.server, "http://localhost:7891");
        return {
          get: async (url) => {
            calls.push(url);
            if (first === "failure" && calls.length === 1)
              throw new Error("network");
            return {
              ok: () => true,
              text: async () => (calls.length === 1 ? first : "192.0.2.21"),
              dispose: async () => {},
            };
          },
          dispose: async () => {},
        };
      },
    });
    assert.equal(ip, first === "192.0.2.20" ? first : "192.0.2.21");
    assert.deepEqual(
      calls,
      first === "192.0.2.20"
        ? ["https://api-ipv4.ip.sb/ip"]
        : ["https://api-ipv4.ip.sb/ip", "https://api.ip.sb/ip"],
    );
  }
});

test("IPv6-only discovery reports unsupported scoring instead of querying a different IP", async () => {
  await assert.rejects(
    resolveExitIP({
      proxyUrl: "http://localhost:7891",
      createContext: () => ({
        get: async () => ({
          ok: () => true,
          text: async () => "2001:db8::1",
          dispose: async () => {},
        }),
        dispose: async () => {},
      }),
    }),
    /暂不支持 IPv6/,
  );
  await assert.rejects(queryIPPure({ target: "2001:db8::1" }), /暂不支持 IPv6/);
});
