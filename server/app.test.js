import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
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
