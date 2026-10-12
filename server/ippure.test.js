import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { summarizeProxy } from "./node-sources.js";
import { startIPPureCollector, queryIPPure } from "./ippure.js";

test("IPPure uses archived full configuration when live subscription is unavailable", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-archived-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const rawConfig = {
    name: "archived",
    type: "http",
    server: "node.example",
    port: 443,
    tls: true,
    username: "private",
    password: "secret",
  };
  const file = path.join(dir, "results.json");
  await writeFile(
    file,
    JSON.stringify({ nodes: [{ ...summarizeProxy(rawConfig), rawConfig }] }),
  );
  const store = createStore();
  let resolutions = 0;
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7890 + i}`])),
    store,
    subDir: dir,
    intervalMs: 999999,
    resolveIP: async ({ proxy }) => {
      assert.deepEqual(proxy, rawConfig);
      resolutions++;
      return "192.0.2.44";
    },
    query: async ({ proxy, target }) => {
      assert.deepEqual(proxy, rawConfig);
      assert.equal(target, "192.0.2.44");
      return { ip: target, asnOrganization: "Test" };
    },
  });
  t.after(async () => {
    await collector.close();
    await store.close();
  });
  await collector.run();
  await store.sync(file);
  await collector.run();
  assert.equal(resolutions, 1);
  assert.equal((await store.get()).nodes[0].ippure.status, "success");
});

test("IPPure saves per-node results, retries failures without rechecking successes and preserves batches after reopen", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-test-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  const nodes = ["good", "bad", "unmatched"].map((name) => ({
    name,
    type: "http",
    server: `${name}.example.org`,
    port: "443",
  }));
  const file = path.join(dir, "results.json");
  const snapshot = { checkedAt: "2026-10-11T00:00:00Z", nodes };
  await writeFile(file, JSON.stringify(snapshot));
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({
      proxies: nodes.slice(0, 2).map((n) => ({ ...n, password: "private" })),
    }),
  );
  const database = path.join(dir, "history.sqlite");
  let store = createStore(database);
  await store.sync(file);
  let calls = [];
  const result = {
    ip: "82.38.64.237",
    asn: "AS212194",
    asnOrganization: "YUUSEI LTD",
    source: "广播IP",
    attributes: ["机房IP"],
    humanPercent: 15.96,
    botPercent: 84.04,
    score: 32,
    label: "中性",
  };
  let failBackfill = false;
  const resolutions = [];
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7890 + i}`])),
    store,
    subDir: dir,
    intervalMs: 999999,
    resolveIP: async ({ proxy }) => {
      resolutions.push(proxy.name);
      return proxy.name === "good" ? result.ip : "192.0.2.2";
    },
    query: async ({ proxy, target }) => {
      calls.push(target === result.ip ? "good" : "bad");
      assert.equal(proxy.password, "private");
      assert.equal(proxy.name, "good");
      if (target === "192.0.2.2" || failBackfill)
        throw new Error("connection failed with private credentials");
      return result;
    },
  });
  await collector.run();
  const first = await store.get();
  assert.deepEqual(calls, ["good", "bad"]);
  assert.equal(first.nodes[0].ippure.score, 32);
  assert.equal(first.nodes[1].ippure.status, "error");
  assert.equal(JSON.stringify(first).includes("private"), false);
  assert.equal(first.nodes[2].ippure, undefined);
  await collector.run();
  assert.equal(calls.length, 2);
  await store.saveIPPure(first.id, 1, {
    ...first.nodes[1].ippure,
    checkedAt: "2020-01-01T00:00:00Z",
  });
  await collector.run();
  assert.deepEqual(calls, ["good", "bad", "bad"]);
  assert.equal((await store.get()).nodes[1].ippure.attempts, 2);
  await store.saveIPPure(first.id, 1, {
    ...(await store.get()).nodes[1].ippure,
    attempts: 3,
    checkedAt: "2020-01-01T00:00:00Z",
  });
  await collector.run();
  assert.equal(calls.length, 3);
  const legacy = { ...(await store.get()).nodes[0].ippure };
  delete legacy.asnOrganization;
  await store.saveIPPure(first.id, 0, legacy);
  failBackfill = true;
  await collector.run();
  const preserved = (await store.get()).nodes[0].ippure;
  assert.equal(preserved.status, "success");
  assert.equal(preserved.score, 32);
  assert.equal(preserved.organizationAttempts, 1);
  await collector.run();
  assert.equal(calls.length, 4); // failed backfill is throttled
  await store.saveIPPure(first.id, 0, {
    ...preserved,
    organizationCheckedAt: "2020-01-01T00:00:00Z",
  });
  failBackfill = false;
  await collector.run();
  assert.equal(
    (await store.get()).nodes[0].ippure.asnOrganization,
    "YUUSEI LTD",
  );
  await collector.run();
  assert.equal(calls.length, 5);
  assert.deepEqual(resolutions, ["good", "bad"]); // retries and backfills reuse the saved exit IP
  await store.sync(file);
  assert.equal((await store.history(10, 0)).total, 1);
  await collector.close();
  await store.close();
  store = createStore(database);
  try {
    assert.equal((await store.get(first.id)).nodes[0].ippure.score, 32);
    assert.equal(
      (await store.get(first.id)).nodes[0].ippure.asnOrganization,
      "YUUSEI LTD",
    );
    await writeFile(
      file,
      JSON.stringify({ ...snapshot, checkedAt: "2026-10-11T01:00:00Z" }),
    );
    await store.sync(file);
    assert.equal((await store.get()).nodes[0].ippure, undefined);
    assert.equal((await store.aggregate()).nodes[0].ippure, undefined);
    assert.equal((await store.get(first.id)).nodes[0].ippure.score, 32);
    assert.equal((await store.history(10, 0)).total, 2);
  } finally {
    await store.close();
  }
});

test("IPPure rotates successful transports, rescues exhausted failures and saves results to the queried node", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-rotation-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  const nodes = ["donor-a", "donor-b", "target-a", "target-b"].map((name) => ({
    name,
    type: "ss",
    server: `${name}.example.org`,
    port: "8388",
  }));
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: nodes }),
  );
  const snapshot = { id: 1, nodes };
  for (const node of nodes.slice(0, 2)) {
    node.ippure = {
      status: "success",
      exitIPSource: "ip.sb",
      asnOrganization: "Existing",
      score: 20,
    };
  }
  nodes[2].ippure = {
    status: "error",
    attempts: 3,
    checkedAt: new Date().toISOString(),
  };
  const calls = [];
  const resolutions = [];
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7890 + i}`])),
    subDir: dir,
    intervalMs: 999999,
    resolveIP: async ({ proxy }) => {
      resolutions.push(proxy.name);
      return proxy.name === "target-a" ? "192.0.2.1" : "192.0.2.2";
    },
    store: {
      get: async () => structuredClone(snapshot),
      saveIPPure: async (id, index, result) => {
        assert.equal(id, 1);
        nodes[index].ippure = result;
      },
    },
    query: async ({ proxy, target }) => {
      calls.push([proxy.name, target]);
      if (proxy.name === "donor-a") throw new Error("transport failed");
      return { ip: target, asnOrganization: "Queried", score: 42 };
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(calls, [
    ["donor-a", "192.0.2.1"],
    ["donor-b", "192.0.2.1"],
    ["donor-b", "192.0.2.2"],
  ]);
  assert.equal(nodes[0].ippure.score, 20);
  assert.equal(nodes[1].ippure.score, 20);
  assert.equal(nodes[2].ippure.ip, "192.0.2.1");
  assert.equal(nodes[3].ippure.ip, "192.0.2.2");
  assert.equal(nodes[2].ippure.queryTarget, "192.0.2.1");
  assert.deepEqual(resolutions, ["target-a", "target-b"]);
  assert.equal(nodes[2].ippure.attempts, 1);
  await collector.run();
  assert.equal(calls.length, 3);
});

test("IPPure uses different successful nodes on consecutive queries", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-round-robin-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  const nodes = ["a", "b", "c", "d"].map((name) => ({
    name,
    type: "http",
    server: `${name}.example.org`,
    port: 443,
  }));
  for (const node of nodes.slice(0, 2))
    node.ippure = {
      status: "success",
      exitIPSource: "ip.sb",
      asnOrganization: "Existing",
    };
  nodes[2].ippure = {
    status: "error",
    ip: "74.82.60.52",
    exitIPSource: "ip.sb",
  };
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: nodes }),
  );
  const calls = [];
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7890 + i}`])),
    subDir: dir,
    intervalMs: 999999,
    resolveIP: async ({ proxy }) => {
      assert.equal(proxy.name, "d");
      return "192.0.2.4";
    },
    store: {
      get: async () => structuredClone({ id: 1, nodes }),
      saveIPPure: async (_id, index, result) => {
        nodes[index].ippure = result;
      },
    },
    query: async ({ proxy, target }) => {
      calls.push([proxy.name, target]);
      return { ip: target, asnOrganization: "Queried", score: 10 };
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(calls, [
    ["a", "74.82.60.52"],
    ["b", "192.0.2.4"],
  ]);
});

test("IPPure refreshes legacy results and reuses only an IP.SB-verified exit IP", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-exit-source-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  const node = {
    name: "target",
    type: "ss",
    server: "node.example.org",
    port: 8388,
    ippure: {
      status: "success",
      queryMode: "specified-exit",
      ip: "8.8.8.8",
      asnOrganization: "Legacy result",
      score: 7,
    },
  };
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: [{ ...node, password: "private" }] }),
  );
  const snapshot = { id: 1, nodes: [node] };
  const calls = [];
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7890 + i}`])),
    subDir: dir,
    intervalMs: 999999,
    resolveIP: async ({ proxy }) => {
      calls.push(["resolve", proxy.name]);
      return "203.0.113.7";
    },
    store: {
      get: async () => structuredClone(snapshot),
      saveIPPure: async (_id, index, result) => {
        node.ippure = result;
      },
    },
    query: async ({ target }) => {
      calls.push(["query", target]);
      return {
        ip: target,
        asnOrganization: "Verified result",
        score: 12,
      };
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(calls, [
    ["resolve", "target"],
    ["query", "203.0.113.7"],
  ]);
  assert.equal(node.ippure.ip, "203.0.113.7");
  assert.equal(node.ippure.exitIPSource, "ip.sb");
});

test("IPPure refuses a hostname as the explicit query target before changing proxy configuration", async () => {
  await assert.rejects(
    queryIPPure({ target: "voteshirleysew.club" }),
    /查询目标必须是出口 IP/,
  );
});

test("IPPure does not query the server address when resolving the exit IP fails", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-no-exit-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  const node = {
    name: "target",
    type: "http",
    server: "192.0.2.99",
    port: 443,
  };
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: [node] }),
  );
  let queries = 0;
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7890 + i}`])),
    subDir: dir,
    intervalMs: 999999,
    store: {
      get: async () => structuredClone({ id: 1, nodes: [node] }),
      saveIPPure: async (_id, _index, result) => {
        node.ippure = result;
      },
    },
    resolveIP: async () => {
      throw new Error("IPPure 无法获取节点出口 IP");
    },
    query: async () => {
      queries++;
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.equal(queries, 0);
  assert.equal(node.ippure.status, "error");
  assert.equal(node.ippure.ip, undefined);
  assert.equal(node.ippure.error, "IPPure 无法获取节点出口 IP");
});

test("batch collection runs concurrently, deduplicates exit IPs and drains before reconfiguration", async (t) => {
  const nodes = Array.from({ length: 7 }, (_, i) => ({
    name: `node${i}`,
    rawConfig: {
      name: `node${i}`,
      type: "http",
      server: `node${i}.example`,
      port: 443,
    },
  }));
  let active = 0;
  let maxActive = 0;
  let configurations = 0;
  const calls = [];
  const collector = startIPPureCollector({
    concurrency: 3,
    intervalMs: 999999,
    store: {
      get: async () => structuredClone({ id: 1, nodes }),
      saveIPPure: async (_id, index, data) => {
        nodes[index].ippure = data;
      },
    },
    configure: async ({ proxies }) => {
      assert.equal(active, 0);
      assert.ok(proxies.length <= 6);
      configurations++;
      return new Map(
        proxies.map((proxy, i) => [proxy, `http://localhost:${7890 + i}`]),
      );
    },
    resolveIP: async ({ proxy, proxyUrl }) => {
      assert.ok(proxyUrl);
      active++;
      maxActive = Math.max(maxActive, active);
      await new Promise((r) => setTimeout(r, 10));
      active--;
      return ["node0", "node1", "node3"].includes(proxy.name)
        ? "192.0.2.1"
        : `192.0.2.${Number(proxy.name.slice(4)) + 10}`;
    },
    query: async ({ target, proxyUrl }) => {
      assert.ok(proxyUrl);
      calls.push(target);
      active++;
      maxActive = Math.max(maxActive, active);
      await new Promise((r) => setTimeout(r, 10));
      active--;
      return { ip: target, asnOrganization: "Test", score: 36 };
    },
  });
  t.after(() => collector.close());
  await Promise.all([collector.run(), collector.run()]);
  assert.equal(maxActive, 3);
  assert.equal(configurations, 3);
  assert.equal(calls.filter((ip) => ip === "192.0.2.1").length, 1);
  assert.equal(
    nodes.filter((node) => node.ippure.status === "success").length,
    7,
  );
  assert.equal(nodes[3].ippure.ip, "192.0.2.1");
  await collector.run();
  assert.equal(configurations, 3);
});

test("a changed snapshot prevents stale concurrent writes and stops further batches", async (t) => {
  let snapshotId = 1;
  let configured = 0;
  const nodes = Array.from({ length: 4 }, (_, i) => ({
    rawConfig: { name: String(i) },
  }));
  const collector = startIPPureCollector({
    concurrency: 2,
    intervalMs: 999999,
    store: {
      get: async () => ({ id: snapshotId, nodes }),
      saveIPPure: async () => assert.fail("stale result written"),
    },
    configure: async ({ proxies }) => {
      configured++;
      return new Map(proxies.map((p) => [p, "http://localhost:7890"]));
    },
    resolveIP: async () => "192.0.2.1",
    query: async ({ target }) => {
      snapshotId = 2;
      return { ip: target };
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.equal(configured, 1);
});

test("failed saves do not release the batch until all concurrent requests settle", async (t) => {
  const nodes = [0, 1].map((i) => ({ rawConfig: { name: String(i) } }));
  let release;
  const blocked = new Promise((r) => {
    release = r;
  });
  let completed = false;
  const collector = startIPPureCollector({
    concurrency: 2,
    intervalMs: 999999,
    store: {
      get: async () => ({ id: 1, nodes }),
      saveIPPure: async () => {
        throw new Error("database unavailable");
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7890"])),
    resolveIP: async ({ proxy }) => `192.0.2.${Number(proxy.name) + 1}`,
    query: async ({ target }) => {
      if (target.endsWith("2")) await blocked;
      return { ip: target };
    },
  });
  t.after(() => collector.close());
  const running = collector.run().then(() => {
    completed = true;
  });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(completed, false);
  release();
  await running;
  assert.match(collector.status().error, /采集暂时不可用/);
});

test("HTTP migration gives exhausted browser failures a fresh retry budget and keeps verified exit IPs", async (t) => {
  const node = {
    rawConfig: { name: "old-browser-failure" },
    ippure: {
      status: "error",
      ip: "192.0.2.1",
      exitIPSource: "ip.sb",
      exitIPMode: "ip.sb",
      queryMode: "specified-exit",
      attempts: 3,
      checkedAt: new Date().toISOString(),
    },
  };
  const collector = startIPPureCollector({
    intervalMs: 999999,
    store: {
      get: async () => ({ id: 1, nodes: [node] }),
      saveIPPure: async (_id, _index, data) => {
        node.ippure = data;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7890"])),
    resolveIP: async () => assert.fail("verified exit IP should be reused"),
    query: async ({ target }) => ({ ip: target, asnOrganization: "Updated" }),
  });
  t.after(() => collector.close());
  await collector.run();
  assert.equal(node.ippure.status, "success");
  assert.equal(node.ippure.attempts, 1);
  assert.equal(node.ippure.transportMode, "http");
});
