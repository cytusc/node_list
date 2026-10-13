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
  resolveExitCountry,
  resolveExitIP,
  resolveExitIPInfo,
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
    // Every success is followed by one same-tunnel trace request for the
    // exit country; its body here carries no loc code, which is tolerated.
    const trace = "https://www.cloudflare.com/cdn-cgi/trace";
    assert.deepEqual(
      calls,
      first === "192.0.2.20"
        ? ["https://api-ipv4.ip.sb/ip", trace]
        : ["https://api-ipv4.ip.sb/ip", "https://checkip.amazonaws.com/", trace],
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
    /IPPure 风险评分接口需要 IPv4/,
  );
  await assert.rejects(queryIPPure({ target: "2001:db8::1" }), /暂不支持 IPv6/);
});

test("exit discovery retains IPv6 when IPv4 services fail and records the actual observer", async () => {
  const calls = [];
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async (url) => {
        calls.push(url);
        if (url === "https://checkip.amazonaws.com/")
          return response("2001:db8::23");
        throw new Error("unreachable");
      },
      dispose: async () => {},
    }),
  });
  assert.deepEqual(data, {
    ip: "2001:db8::23",
    provider: "AWS CheckIP",
    addressFamily: 6,
  });
  assert.ok(calls.includes("https://api.ipify.org"));
});

test("Cloudflare trace is parsed without confusing metadata with an exit address", async () => {
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async (url) => {
        if (!url.includes("cdn-cgi/trace")) throw new Error("unreachable");
        return response("fl=123\nip=192.0.2.23\nloc=SG\nwarp=on\n");
      },
      dispose: async () => {},
    }),
  });
  assert.deepEqual(data, {
    ip: "192.0.2.23",
    provider: "Cloudflare",
    addressFamily: 4,
    country: "SG",
  });
});

test("a non-trace exit success rides the same tunnel once to learn the country", async () => {
  const calls = [];
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async (url) => {
        calls.push(url);
        if (url.includes("cdn-cgi/trace"))
          return response("fl=1\nip=192.0.2.99\nloc=JP\nts=1\n");
        return response("192.0.2.42");
      },
      dispose: async () => {},
    }),
  });
  assert.equal(data.ip, "192.0.2.42");
  assert.equal(data.provider, "IP.SB");
  assert.equal(data.country, "JP");
  assert.deepEqual(calls, [
    "https://api-ipv4.ip.sb/ip",
    "https://www.cloudflare.com/cdn-cgi/trace",
  ]);
});

test("a failed country follow-up never fails the exit probe", async () => {
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async (url) => {
        if (url.includes("cdn-cgi/trace")) return response("blocked", 403);
        return response("192.0.2.42");
      },
      dispose: async () => {},
    }),
  });
  assert.deepEqual(data, {
    ip: "192.0.2.42",
    provider: "IP.SB",
    addressFamily: 4,
  });
});

test("resolveExitCountry returns the two-letter trace location", async () => {
  const country = await resolveExitCountry({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async () => response("fl=1\nip=192.0.2.99\nloc=de\n"),
      dispose: async () => {},
    }),
  });
  assert.equal(country, "DE");
});

test("an IP.SB timeout is followed by AWS on a fresh session through the same node", async () => {
  const contexts = [],
    attempts = [];
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    onProbeAttempt: (attempt) => attempts.push(attempt),
    createContext: ({ proxy, timeout }) => {
      assert.equal(proxy.server, "http://localhost:7891");
      assert.equal(timeout, 8000);
      const record = { calls: 0, disposed: false };
      contexts.push(record);
      return {
        get: async (url) => {
          record.calls++;
          if (url.includes("ip.sb")) {
            const error = new Error("sensitive transport message");
            error.cause = { code: "UND_ERR_CONNECT_TIMEOUT" };
            throw error;
          }
          // The country follow-up intentionally reuses the success tunnel.
          assert.equal(
            record.calls,
            url.includes("cdn-cgi/trace") ? 2 : 1,
          );
          assert.equal(contexts[0].disposed, true);
          if (url.includes("cdn-cgi/trace"))
            return response("fl=1\nip=192.0.2.42\nloc=SG\n");
          return response("192.0.2.42");
        },
        dispose: async () => {
          record.disposed = true;
        },
      };
    },
  });
  assert.equal(data.ip, "192.0.2.42");
  assert.equal(data.provider, "AWS CheckIP");
  assert.equal(data.country, "SG");
  assert.equal(contexts.length, 2);
  assert.ok(contexts.every((context) => context.disposed));
  assert.equal(attempts[0].code, "TIMEOUT");
  assert.equal(attempts[1].outcome, "ipv4");
  assert.equal(JSON.stringify(attempts).includes("sensitive"), false);
});

test("exhausted providers are retried once with fresh connections and recover a transient failure", async () => {
  const attempts = [],
    contexts = [];
  const result = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    onProbeAttempt: (attempt) => attempts.push(attempt),
    createContext: () => {
      const attempt = contexts.length;
      contexts.push(attempt);
      return {
        get: async (url) => {
          if (attempt === 9) {
            assert.equal(url, "https://checkip.amazonaws.com/");
            return response("192.0.2.43");
          }
          return response("temporarily unavailable", 503);
        },
        dispose: async () => {},
      };
    },
  });
  assert.equal(result.ip, "192.0.2.43");
  assert.equal(contexts.length, 10);
  assert.equal(attempts.at(-1).round, 2);
  assert.equal(attempts[0].code, "HTTP_503");
});

test("all provider failures produce bounded credential-free diagnostics", async () => {
  const attempts = [];
  await assert.rejects(
    resolveExitIPInfo({
      proxyUrl: "http://user:private@localhost:7891",
      onProbeAttempt: (attempt) => attempts.push(attempt),
      createContext: () => ({
        get: async () => {
          throw new Error("connection failed for user:private");
        },
        dispose: async () => {},
      }),
    }),
    (error) => {
      assert.equal(error.exitIPFailure, true);
      assert.match(error.message, /8 个查询入口/);
      assert.equal(error.message.includes("private"), false);
      return true;
    },
  );
  assert.equal(attempts.length, 11);
  assert.ok(attempts.every((attempt) => attempt.code === "NETWORK_ERROR"));
  assert.equal(JSON.stringify(attempts).includes("private"), false);
});

test("two consecutive transport-level failures stop spending the endpoint budget", async () => {
  const attempts = [];
  await assert.rejects(
    resolveExitIPInfo({
      proxyUrl: "http://localhost:7891",
      createContext: () => ({
        get: async (url) => {
          attempts.push(url);
          throw Object.assign(new Error("reset"), {
            exitProbeCode: "CONNECTION_RESET",
          });
        },
        dispose: async () => {},
      }),
    }),
    /已提前中止/,
  );
  // Two dials instead of the full eight-endpoint budget plus retry round.
  assert.equal(attempts.length, 2);
});

test("a transport-level failure does not abort while a later endpoint still answers", async () => {
  const attempts = [];
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async (url) => {
        attempts.push(url);
        if (attempts.length === 1)
          throw Object.assign(new Error("reset"), {
            exitProbeCode: "CONNECTION_RESET",
          });
        return response("192.0.2.7");
      },
      dispose: async () => {},
    }),
  });
  assert.equal(data.ip, "192.0.2.7");
  assert.ok(attempts.length >= 2);
});

test("HTTP-level errors reset the transport streak because the tunnel is alive", async () => {
  const attempts = [];
  const data = await resolveExitIPInfo({
    proxyUrl: "http://localhost:7891",
    createContext: () => ({
      get: async (url) => {
        attempts.push(url);
        if (url === "https://checkip.amazonaws.com/")
          return response("192.0.2.8");
        const error = new Error("blocked");
        error.exitProbeCode = "HTTP_403";
        throw error;
      },
      dispose: async () => {},
    }),
  });
  assert.equal(data.ip, "192.0.2.8");
  assert.deepEqual(attempts, [
    "https://api-ipv4.ip.sb/ip",
    "https://checkip.amazonaws.com/",
    "https://www.cloudflare.com/cdn-cgi/trace",
  ]);
});

test("normalizeIPPure carries the API country when present and omits it otherwise", () => {
  const withCountry = normalizeIPPure(
    "23.22.22.12",
    { ...basic(), country: "sg" },
    { risk_score: 9 },
    { human: 77.78, bot: 22.22 },
  );
  assert.equal(withCountry.country, "SG");
  const without = normalizeIPPure(
    "23.22.22.12",
    basic(),
    { risk_score: 9 },
    null,
  );
  assert.equal("country" in without, false);
  const invalid = normalizeIPPure(
    "23.22.22.12",
    { ...basic(), country: "SGP" },
    { risk_score: 9 },
    null,
  );
  assert.equal("country" in invalid, false);
});
