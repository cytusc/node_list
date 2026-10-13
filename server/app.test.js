import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { sourceKey, summarizeProxy } from "./node-sources.js";
import { createApp } from "./app.js";

async function fixture(t, options = {}) {
  const subDir = await mkdtemp(path.join(os.tmpdir(), "node-explorer-"));
  const resultsFile = path.join(subDir, "results.json");
  const app = createApp({ subDir, resultsFile, ...options });
  const server = app.listen(0, "127.0.0.1");
  await new Promise((resolve) => server.once("listening", resolve));
  t.after(async () => {
    await new Promise((resolve) => server.close(resolve));
    await app.locals.close();
    await rm(subDir, { recursive: true, force: true });
  });
  return {
    subDir,
    resultsFile,
    get: (url) => fetch(`http://127.0.0.1:${server.address().port}${url}`),
  };
}

test("reads each snapshot afresh and strips credential fields", async (t) => {
  const f = await fixture(t);
  const snapshot = {
    checkedAt: "2026-10-11T08:06:01+08:00",
    speedTest: true,
    mediaCheck: false,
    nodes: [
      {
        name: "HK 1",
        server: "example.org",
        type: "ss",
        speed: 1000,
        password: "secret",
        uuid: "private",
      },
    ],
  };
  await writeFile(f.resultsFile, JSON.stringify(snapshot));
  let response = await f.get("/api/results");
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("cache-control"), "no-store");
  let result = await response.json();
  assert.equal(result.nodes[0].name, "HK 1");
  assert.equal("password" in result.nodes[0], false);
  assert.equal("uuid" in result.nodes[0], false);
  await writeFile(f.resultsFile, JSON.stringify({ ...snapshot, nodes: [] }));
  result = await (await f.get("/api/results")).json();
  assert.deepEqual(result.nodes, []);
});

test("stores share links, backfills matching history and removes untraceable nodes", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "node-links-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const file = path.join(dir, "results.json");
  const linksFile = path.join(dir, "base64.txt");
  const databasePath = path.join(dir, "history", "nodes.sqlite");
  const link = "ss://cipher:secret@node.example:443#Keep";
  const node = {
    name: "Keep",
    baseName: "Keep",
    type: "ss",
    server: "node.example",
    port: "443",
    network: "tcp",
    tls: false,
    udp: false,
    details: [],
  };
  const store = createStore(databasePath);
  try {
    await writeFile(
      file,
      JSON.stringify({
        checkedAt: "2026-10-10T00:00:00Z",
        nodes: [node, { ...node, name: "Untraceable", server: "old.example" }],
      }),
    );
    await store.sync(file);
    const old = await store.get();
    await store.saveIPPure(old.id, 0, { status: "success", ip: "192.0.2.1" });
    await store.saveIPPure(old.id, 1, { status: "success", ip: "192.0.2.2" });

    await writeFile(
      file,
      JSON.stringify({
        checkedAt: "2026-10-11T00:00:00Z",
        nodes: [node],
      }),
    );
    await writeFile(linksFile, Buffer.from(`${link}\n`).toString("base64"));
    await store.sync(file, linksFile);

    assert.equal((await store.get(old.id)).nodes.length, 2);
    await store.reconcileSources(
      new Map([[sourceKey(node), { rawLink: link }]]),
      { prune: true },
    );
    const history = await store.history(10, 0);
    assert.equal(history.total, 2);
    assert.deepEqual(
      (await store.get(old.id)).nodes.map(({ name, rawLink }) => ({
        name,
        rawLink,
      })),
      [{ name: "Keep", rawLink: link }],
    );
    assert.deepEqual((await store.get(old.id)).nodes[0].ippure, {
      status: "success",
      ip: "192.0.2.1",
    });
    assert.equal((await store.get()).nodes[0].rawLink, link);
  } finally {
    await store.close();
  }
});

test("API refreshes always archive node sources, including after the initial import", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "node-api-sources-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const clashFile = path.join(dir, "all.yaml");
  const linksFile = path.join(dir, "base64.txt");
  const proxy = {
    name: "Keep",
    type: "ss",
    server: "node.example",
    port: 443,
    cipher: "aes-256-gcm",
    password: "secret",
  };
  await writeFile(clashFile, JSON.stringify({ proxies: [proxy] }));
  const f = await fixture(t, {
    clashFile,
    linksFile,
    importIntervalMs: 999999,
  });
  for (const [index, route] of [
    "/api/results",
    "/api/results?view=aggregate",
    "/api/history",
  ].entries()) {
    await writeFile(
      f.resultsFile,
      JSON.stringify({
        checkedAt: `2026-10-12T0${index}:00:00Z`,
        nodes: [summarizeProxy(proxy)],
      }),
    );
    assert.equal((await f.get(route)).status, 200);
    const result = await (await f.get("/api/results")).json();
    assert.deepEqual(result.nodes[0].rawConfig, proxy);
    assert.equal(result.nodes.length, 1);
  }
  assert.equal((await (await f.get("/api/history")).json()).total, 3);
});

test("distinguishes missing results from corrupted files without leaking paths", async (t) => {
  const f = await fixture(t);
  let response = await f.get("/api/results");
  assert.equal(response.status, 404);
  assert.equal((await response.json()).code, "NO_RESULTS");
  await writeFile(f.resultsFile, "{broken");
  response = await f.get("/api/results");
  assert.equal(response.status, 503);
  assert.equal((await response.json()).code, "READ_FAILED");
});

test("downloads only allowlisted output files and reports missing formats", async (t) => {
  const f = await fixture(t);
  await mkdir(path.join(f.subDir, "output"));
  await writeFile(path.join(f.subDir, "output", "all.yaml"), "proxies: []\n");
  const list = await (await f.get("/api/downloads")).json();
  assert.equal(list.find((d) => d.id === "clash").available, true);
  assert.equal(list.find((d) => d.id === "mihomo").available, false);
  const response = await f.get("/api/download/clash");
  assert.equal(response.status, 200);
  assert.match(response.headers.get("content-disposition"), /all.yaml/);
  assert.equal(await response.text(), "proxies: []\n");
  assert.equal((await f.get("/api/download/mihomo")).status, 404);
  assert.equal((await f.get("/api/download/constructor")).status, 404);
  assert.equal((await f.get("/api/download/%2e%2e%2fconfig.yaml")).status, 404);
});

test("preserves overwritten batches, deduplicates imports and serves history without source", async (t) => {
  const f = await fixture(t);
  const first = {
    checkedAt: "2026-10-11T00:00:00Z",
    nodes: [{ name: "Old node", password: "secret" }],
  };
  await writeFile(f.resultsFile, JSON.stringify(first));
  const old = await (await f.get("/api/results")).json();
  await f.get("/api/results");
  assert.equal((await (await f.get("/api/history")).json()).total, 1);
  await writeFile(
    f.resultsFile,
    JSON.stringify({ ...first, checkedAt: "2026-10-11T02:00:00Z", nodes: [] }),
  );
  const latest = await (await f.get("/api/results")).json();
  assert.notEqual(latest.id, old.id);
  const history = await (await f.get("/api/history?limit=1&offset=1")).json();
  assert.equal(history.total, 2);
  assert.equal(history.items[0].id, old.id);
  assert.equal(history.items[0].nodeCount, 1);
  await writeFile(f.resultsFile, "{broken");
  const saved = await (await f.get(`/api/results?snapshot=${old.id}`)).json();
  assert.equal(saved.nodes[0].name, "Old node");
  assert.equal("password" in saved.nodes[0], false);
  assert.equal(saved.sourceWarning, "READ_FAILED");
  await rm(f.resultsFile);
  assert.equal((await (await f.get("/api/results")).json()).id, latest.id);
  assert.equal((await f.get("/api/results?snapshot=9999")).status, 404);
  assert.equal((await f.get("/api/results?snapshot=abc")).status, 400);
  assert.equal((await f.get("/api/history?limit=201")).status, 400);
  assert.equal((await f.get("/api/history?offset=-1")).status, 400);
});

test("background collection saves results without page visits and survives database reopen", async (t) => {
  const subDir = await mkdtemp(path.join(os.tmpdir(), "node-history-"));
  t.after(() => rm(subDir, { recursive: true, force: true }));
  const resultsFile = path.join(subDir, "results.json");
  const databasePath = path.join(subDir, "history", "nodes.sqlite");
  const { createStore } = await import("./store.js");
  let app = createApp({
    subDir,
    resultsFile,
    databasePath,
    importIntervalMs: 20,
  });
  t.after(async () => {
    if (app) await app.locals.close();
  });
  await writeFile(
    resultsFile,
    JSON.stringify({
      checkedAt: "2026-10-11T00:00:00Z",
      nodes: [{ name: "Archived" }],
    }),
  );
  // Observe the database directly; no HTTP request should trigger collection.
  const observer = createStore(databasePath);
  try {
    const deadline = Date.now() + 3000;
    while (observer.history(10, 0).total === 0 && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    assert.equal(observer.history(10, 0).total, 1);
  } finally {
    observer.close();
  }
  await app.locals.close();
  app = null;
  await rm(resultsFile);
  const reopened = createStore(databasePath);
  try {
    assert.equal((await reopened.get()).nodes[0].name, "Archived");
    assert.equal(reopened.history(10, 0).total, 1);
  } finally {
    reopened.close();
  }
});

test("database outages return JSON errors without leaking connection details", async (t) => {
  const store = {
    sync: async () => {
      throw new Error("private connection info");
    },
    get: async () => {
      throw new Error("private connection info");
    },
    history: async () => {
      throw new Error("private connection info");
    },
    close: async () => {},
  };
  const f = await fixture(t, { store });
  for (const endpoint of ["/api/results", "/api/history"]) {
    const response = await f.get(endpoint);
    assert.equal(response.status, 503);
    const body = await response.json();
    assert.equal(JSON.stringify(body).includes("private"), false);
  }
});

test("aggregates all batches by connection, keeping latest checks and historical-only nodes", async (t) => {
  const f = await fixture(t);
  const shared = {
    name: "Original",
    type: "vless",
    server: "node.example.org",
    port: "443",
    network: "ws",
    tls: true,
    details: [
      { k: "ws-path", v: "/proxy" },
      { k: "alpn", v: "h2" },
    ],
  };
  const save = async (snapshot) => {
    await writeFile(f.resultsFile, JSON.stringify(snapshot));
    return (await f.get("/api/results")).json();
  };
  const first = await save({
    checkedAt: "2026-10-10T00:00:00Z",
    speedTest: true,
    mediaCheck: true,
    nodes: [
      {
        ...shared,
        speed: 1000,
        media: [{ platform: "openai", tag: "US" }],
        password: "private",
      },
      {
        ...shared,
        server: "archive.example.org",
        name: "Archived",
        speed: 500,
      },
      {
        ...shared,
        details: [{ k: "ws-path", v: "/other" }],
        name: "Other transport",
      },
    ],
  });
  const latest = await save({
    checkedAt: "2026-10-11T00:00:00Z",
    speedTest: false,
    mediaCheck: false,
    nodes: [
      {
        ...shared,
        name: "Renamed",
        port: 443,
        details: [...shared.details].reverse(),
        speed: 0,
        ip: "192.0.2.1",
      },
    ],
  });
  // A late import of an older check must not replace a newer measurement.
  await save({
    checkedAt: "2026-10-09T00:00:00Z",
    nodes: [{ ...shared, speed: 2000 }],
  });
  let response = await f.get("/api/results?view=aggregate");
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("cache-control"), "no-store");
  const result = await response.json();
  assert.equal(result.batchCount, 3);
  assert.equal(result.nodes.length, 3);
  assert.equal(result.checkedAt, latest.checkedAt);
  const updated = result.nodes.find((node) => node.name === "Renamed");
  assert.equal(updated.speed, 0);
  assert.equal(updated.mediaCheck, false);
  assert.equal(updated.snapshotId, latest.id);
  assert.equal(updated.checkedAt, latest.checkedAt);
  assert.equal(updated.media, undefined);
  assert.equal(
    result.nodes.find((node) => node.name === "Archived").snapshotId,
    first.id,
  );
  assert.equal(result.speedTest, true);
  assert.equal(result.mediaCheck, true);
  assert.equal(JSON.stringify(result).includes("private"), false);
  await rm(f.resultsFile);
  const saved = await (await f.get("/api/results?view=aggregate")).json();
  assert.equal(saved.nodes.length, 3);
  assert.equal(saved.sourceWarning, "NO_RESULTS");
  assert.equal(
    (await f.get("/api/results?view=aggregate&snapshot=1")).status,
    400,
  );
  assert.equal((await f.get("/api/results?view=unknown")).status, 400);
});

test("empty aggregated results distinguish no history from empty saved batches", async (t) => {
  const f = await fixture(t);
  assert.equal((await f.get("/api/results?view=aggregate")).status, 404);
  await writeFile(
    f.resultsFile,
    JSON.stringify({ nodes: [], speedTest: true }),
  );
  const result = await (await f.get("/api/results?view=aggregate")).json();
  assert.deepEqual(result.nodes, []);
  assert.equal(result.batchCount, 1);
  assert.equal(result.speedTest, true);
});

test("result pages filter and sort the complete aggregate before slicing, with global statistics", async (t) => {
  const f = await fixture(t);
  await writeFile(
    f.resultsFile,
    JSON.stringify({
      speedTest: true,
      mediaCheck: true,
      nodes: Array.from({ length: 123 }, (_, i) => ({
        name: `Node ${i + 1}`,
        server: `node-${i + 1}.example.org`,
        port: "443",
        type: i % 2 ? "vless" : "http",
        country: i % 2 ? "JP" : "US",
        network: "tcp",
        tls: true,
        udp: i % 2 === 1,
        speed: (i + 1) * 1024,
        media: [{ platform: "openai", tag: i % 2 ? "US" : "" }],
        ippure: i === 0 ? undefined : { status: "success", score: i },
      })),
    }),
  );
  const get = async (query) =>
    (await f.get(`/api/results?view=aggregate&${query}`)).json();
  const first = await get("page=1&limit=50&sort=speed&direction=desc");
  assert.equal(first.nodes.length, 50);
  assert.equal(first.nodes[0].name, "Node 123");
  assert.equal(first.total, 123);
  assert.equal(first.totalNodes, 123);
  assert.equal(first.summary.maxSpeed, 123 * 1024);
  assert.equal(first.summary.median, 62 * 1024);
  assert.deepEqual(
    first.facets.country.map((v) => v.count),
    [62, 61],
  );
  const second = await get("page=2&limit=50&sort=speed&direction=desc");
  assert.equal(second.nodes.length, 50);
  assert.equal(second.nodes[0].name, "Node 73");
  assert.equal(second.page, 2);
  assert.deepEqual(second.summary, first.summary);
  const last = await get("page=3&limit=50&sort=speed&direction=desc");
  assert.equal(last.nodes.length, 23);
  assert.equal(last.nodes.at(-1).name, "Node 1");
  const filtered = await get(
    "page=1&limit=20&type=vless&country=JP&features=tls,udp&media=openai&minSpeed=60&sort=speed&direction=asc",
  );
  assert.equal(filtered.total, 32);
  assert.equal(filtered.nodes.length, 20);
  assert.equal(filtered.nodes[0].name, "Node 60");
  assert.equal(filtered.totalNodes, 123);
  assert.deepEqual(filtered.facets, first.facets);
  const search = await get("page=2&limit=50&q=node-5.example.org");
  assert.equal(search.page, 1);
  assert.equal(search.total, 1);
  assert.equal(search.nodes[0].name, "Node 5");
  const none = await get("page=2&limit=50&q=missing-node");
  assert.equal(none.page, 1);
  assert.equal(none.total, 0);
  assert.deepEqual(none.nodes, []);
  for (const query of [
    "page=0",
    "page=1.5",
    "limit=201",
    "limit=-1",
    "page=1&sort=invalid",
    "page=1&direction=invalid",
    "page=1&minSpeed=-1",
    "page=1&features=invalid",
    "page=1&q=a&q=b",
  ]) {
    assert.equal((await f.get(`/api/results?${query}`)).status, 400, query);
  }
});

test(
  "database pagination and history do not wait for background import or call full aggregation",
  { timeout: 3000 },
  async (t) => {
    let release;
    const importing = new Promise((resolve) => {
      release = resolve;
    });
    t.after(() => release());
    let syncCalls = 0;
    const page = {
      nodes: [{ name: "Page node" }],
      page: 2,
      limit: 1,
      total: 3,
    };
    const store = {
      sync: async () => {
        syncCalls++;
        await importing;
      },
      aggregate: async () => {
        throw new Error("Full history must not be read");
      },
      aggregatePage: async (query) => {
        assert.equal(query.page, 2);
        assert.equal(query.limit, 1);
        return page;
      },
      refreshAggregate: async () => {},
      history: async () => ({ items: [], total: 0 }),
      close: async () => {},
    };
    const f = await fixture(t, { store, importIntervalMs: 999999 });
    try {
      const response = await f.get(
        "/api/results?view=aggregate&page=2&limit=1",
      );
      assert.equal(response.status, 200);
      assert.deepEqual((await response.json()).nodes, page.nodes);
      assert.equal((await f.get("/api/history")).status, 200);
      assert.equal(syncCalls, 1);
    } finally {
      release();
    }
  },
);
