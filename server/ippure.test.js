import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { startIPPureCollector, queryIPPure } from "./ippure.js";

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
    node.ippure = { status: "success", asnOrganization: "Existing", score: 20 };
  }
  nodes[2].ippure = {
    status: "error",
    attempts: 3,
    checkedAt: new Date().toISOString(),
  };
  const calls = [];
  const resolutions = [];
  const collector = startIPPureCollector({
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
    node.ippure = { status: "success", asnOrganization: "Existing" };
  nodes[2].ippure = { status: "error", ip: "74.82.60.52" };
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: nodes }),
  );
  const calls = [];
  const collector = startIPPureCollector({
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
