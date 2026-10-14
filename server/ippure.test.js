import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { aggregateIdentity, summarizeProxy } from "./node-sources.js";
import { startIPPureCollector, queryIPPure } from "./ippure.js";
import { EXIT_IP_VERSION, EXIT_IP_PROBE_VERSION } from "./exit-ip.js";

test("legacy exit failures recover before pending links and persist provider diagnostics without rechecking verified exits", async (t) => {
  const id = "12345678-1234-1234-1234-123456789abc";
  const old = {
    name: "recover",
    type: "vless",
    server: "recover.example",
    port: "443",
    rawLink: `vless://${id}@recover.example:443`,
    snapshotId: 1,
    ippure: {
      status: "error",
      failureStage: "exit-ip",
      attempts: 3,
      checkedAt: new Date().toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: 3,
    },
  };
  const fresh = {
    ...old,
    name: "fresh",
    server: "fresh.example",
    rawLink: `vless://${id}@fresh.example:443`,
    ippure: undefined,
  };
  const donor = {
    name: "donor",
    rawConfig: { name: "donor" },
    snapshotId: 1,
    ippure: {
      status: "success",
      ip: "192.0.2.10",
      exitIPMode: "proxy-echo",
      exitIPVersion: 3,
      exitIPSource: "AWS CheckIP",
      asnOrganization: "Verified",
    },
  };
  const nodes = [fresh, old, donor],
    calls = [],
    saves = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (id, index, result) => {
        saves.push(result);
        nodes[index].ippure = result;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy, onProbeAttempt }) => {
      calls.push(proxy.name);
      onProbeAttempt({ provider: "IP.SB", outcome: "error", code: "TIMEOUT" });
      onProbeAttempt({ provider: "AWS CheckIP", outcome: "ipv4" });
      return { ip: "192.0.2.11", provider: "AWS CheckIP", addressFamily: 4 };
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(calls, ["recover", "fresh"]);
  assert.equal(saves[0].status, "success");
  assert.equal(saves[0].attempts, 1);
  assert.equal(saves[0].exitIPProbeVersion, 1);
  assert.equal(saves[0].exitIPSource, "AWS CheckIP");
  assert.equal(saves[0].exitIPAttempts[0].code, "TIMEOUT");
  await collector.run();
  assert.equal(calls.length, 2);
});

test("Qilan share links enter historical collection without all.yaml and reuse results after refresh", async (t) => {
  const store = createStore();
  const id = "12345678-1234-1234-1234-123456789abc";
  const link = `vless://${id}@example.com:443?security=tls&type=ws&host=ws.example&path=%2Fws#Qilan`;
  const node = {
    name: "Qilan",
    subTag: "Qilan",
    type: "vless",
    server: "example.com",
    port: "443",
    rawLink: link,
  };
  await store.syncSnapshot({
    checkedAt: "2026-10-12T06:00:00Z",
    nodes: [
      node,
      { ...node, name: "Alias", rawLink: link.replace("#Qilan", "#Alias") },
      {
        ...node,
        server: "invalid.example",
        rawLink: "vless://private@invalid.example:443",
      },
    ],
  });
  const archived = await store.get();
  const donor = {
    name: "donor",
    type: "http",
    server: "donor.example",
    port: 443,
  };
  await store.syncSnapshot({
    checkedAt: "2026-10-12T07:00:00Z",
    nodes: [{ ...summarizeProxy(donor), rawConfig: donor }],
  });
  await store.saveIPPure((await store.get()).id, 0, {
    status: "success",
    ip: "192.0.2.5",
    exitIPSource: "ip.sb",
    asnOrganization: "Donor",
  });
  let probes = 0;
  const collector = startIPPureCollector({
    store,
    subDir: "/nonexistent-qilan-subscription",
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7900 + i}`])),
    resolveIP: async ({ proxy }) => {
      probes++;
      assert.equal(proxy.uuid, id);
      assert.deepEqual(proxy["ws-opts"], {
        path: "/ws",
        headers: { Host: "ws.example" },
      });
      return { ip: "192.0.2.44", provider: "AWS CheckIP" };
    },
    query: async ({ proxy, target }) => {
      assert.equal(proxy.name, "donor");
      assert.equal(target, "192.0.2.44");
      return { ip: target, asnOrganization: "Qilan exit" };
    },
  });
  t.after(async () => {
    await collector.close();
    await store.close();
  });
  await collector.run();
  assert.equal(probes, 1);
  assert.equal(
    (await store.get(archived.id)).nodes[0].ippure.status,
    "success",
  );
  assert.equal((await store.get(archived.id)).nodes[1].ippure, undefined); // one probe per connection
  assert.equal(
    (await store.get(archived.id)).nodes[2].ippure.failureStage,
    "share-link",
  );
  await store.syncSnapshot({
    checkedAt: "2026-10-12T08:00:00Z",
    nodes: [
      { ...node, name: "Refreshed", details: [{ k: "latency", v: "90 ms" }] },
    ],
  });
  assert.equal(
    (await store.aggregate()).nodes.find((n) => n.name === "Refreshed").ippure
      .ip,
    "192.0.2.44",
  );
  await collector.run();
  assert.equal(probes, 1);
});

test("nodes with an empty rawLink are marked unsupported instead of silently skipped", async (t) => {
  const store = createStore();
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-empty-link-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: [] }),
  );
  await store.syncSnapshot({
    checkedAt: "2026-10-14T00:00:00Z",
    nodes: [
      {
        name: "LT 03 [Qilan]",
        subTag: "Qilan",
        type: "ss",
        server: "195.238.126.20",
        port: "9068",
        rawLink: "",
      },
    ],
  });
  const archived = await store.get();
  let probes = 0;
  const collector = startIPPureCollector({
    store,
    subDir: dir,
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p, i) => [p, `http://127.0.0.1:${7900 + i}`])),
    resolveIP: async () => {
      probes++;
      return { ip: "192.0.2.1", provider: "AWS CheckIP" };
    },
    query: async () => {
      probes++;
      return { ip: "192.0.2.1", asnOrganization: "Never" };
    },
  });
  t.after(async () => {
    await collector.close();
    await store.close();
  });
  await collector.run();
  assert.equal(probes, 0);
  const saved = (await store.get(archived.id)).nodes[0].ippure;
  assert.equal(saved.status, "unsupported");
  assert.equal(saved.failureStage, "share-link");
  assert.match(saved.error, /分享链接为空/);
  // A marked instance must not re-enter the queue on the next round.
  await collector.run();
  assert.equal(probes, 0);
});

test("one rejected Qilan configuration is isolated while other nodes are probed", async (t) => {
  const store = createStore();
  const id = "12345678-1234-1234-1234-123456789abc";
  await store.syncSnapshot({
    nodes: ["rejected", "working"].map((name) => ({
      name,
      subTag: "Qilan",
      type: "vless",
      server: `${name}.example`,
      port: "443",
      rawLink: `vless://${id}@${name}.example:443?security=tls`,
    })),
  });
  const probes = [];
  const collector = startIPPureCollector({
    store,
    subDir: "/nonexistent",
    intervalMs: 999999,
    configure: async ({ proxies }) => {
      if (proxies.some((p) => p.name === "rejected"))
        throw new Error("代理配置失败 (HTTP 400)");
      return new Map(
        proxies.map((p, i) => [p, `http://127.0.0.1:${7900 + i}`]),
      );
    },
    resolveIP: async ({ proxy }) => {
      probes.push(proxy.name);
      return "192.0.2.44";
    },
    query: async () => ({ asnOrganization: "Test" }),
  });
  t.after(async () => {
    await collector.close();
    await store.close();
  });
  await collector.run();
  assert.deepEqual(probes, ["working"]);
  const snapshot = await store.get();
  assert.equal(snapshot.nodes[0].ippure.failureStage, "share-link");
  assert.equal(snapshot.nodes[1].ippure.status, "success");
  assert.equal(collector.status().error, null);
});

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
  assert.equal(first.nodes[2].ippure.status, "unsupported");
  assert.equal(first.nodes[2].ippure.failureStage, "share-link");
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
  assert.equal(calls.length, 4);
  assert.equal((await store.get()).nodes[1].ippure.attempts, 4);
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
  assert.equal(calls.length, 5); // failed backfill is throttled
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
  assert.equal(calls.length, 6);
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
    // The fresh batch itself carries no result — enabling is per row — but the
    // connection did not change, so the aggregate keeps its latest exit data
    // instead of falling back to 待检测 until the next probe.
    assert.equal((await store.get()).nodes[0].ippure, undefined);
    assert.equal((await store.aggregate()).nodes[0].ippure.score, 32);
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
      country: "SG",
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

test("IPPure refreshes legacy successes from archived batches absent in the latest batch", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-history-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(path.join(dir, "output"));
  const oldConfig = {
    name: "archived-target",
    type: "ss",
    server: "45.149.114.171",
    port: 777,
    cipher: "aes-128-gcm",
    password: "private",
  };
  const oldFile = path.join(dir, "old.json");
  await writeFile(
    oldFile,
    JSON.stringify({ nodes: [{ name: oldConfig.name, rawConfig: oldConfig }] }),
  );
  const store = createStore();
  await store.sync(oldFile);
  const old = await store.get();
  await store.saveIPPure(old.id, 0, {
    status: "success",
    ip: "8.8.8.8",
    asnOrganization: "Google LLC",
    score: 7,
  });

  const donor = {
    name: "current-donor",
    type: "http",
    server: "donor.example.org",
    port: 443,
  };
  const currentFile = path.join(dir, "current.json");
  await writeFile(
    currentFile,
    JSON.stringify({
      nodes: [
        {
          name: donor.name,
          type: donor.type,
          server: donor.server,
          port: donor.port,
        },
      ],
    }),
  );
  await writeFile(
    path.join(dir, "output", "all.yaml"),
    JSON.stringify({ proxies: [donor] }),
  );
  await store.sync(currentFile);
  const current = await store.get();
  await store.saveIPPure(current.id, 0, {
    status: "success",
    ip: "192.0.2.8",
    exitIPSource: "ip.sb",
    asnOrganization: "Donor",
    country: "SG",
  });

  const calls = [];
  const collector = startIPPureCollector({
    concurrency: 1,
    configure: async ({ proxies }) =>
      new Map(
        proxies.map((proxy, index) => [
          proxy,
          `http://127.0.0.1:${7890 + index}`,
        ]),
      ),
    store,
    subDir: dir,
    intervalMs: 999999,
    resolveIP: async ({ proxy }) => {
      assert.equal(proxy.name, "archived-target");
      return "203.0.113.7";
    },
    query: async ({ target }) => {
      calls.push(target);
      return { ip: target, asnOrganization: "Verified", score: 12 };
    },
  });
  t.after(async () => {
    await collector.close();
    await store.close();
  });
  await collector.run();
  assert.deepEqual(calls, ["203.0.113.7"]);
  assert.equal((await store.get(old.id)).nodes[0].ippure.ip, "203.0.113.7");
  assert.equal((await store.get(old.id)).nodes[0].ippure.exitIPSource, "ip.sb");
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
      // A wave carries every pending job plus the donor pool.
      assert.ok(proxies.length <= 10);
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
  // All seven jobs fit in one wave, so a single reload serves the whole run.
  assert.equal(configurations, 1);
  assert.equal(calls.filter((ip) => ip === "192.0.2.1").length, 1);
  assert.equal(
    nodes.filter((node) => node.ippure.status === "success").length,
    7,
  );
  assert.equal(nodes[3].ippure.ip, "192.0.2.1");
  await collector.run();
  assert.equal(configurations, 1);
});

test("a changed snapshot keeps completed results and drains the remaining batches", async (t) => {
  let snapshotId = 1;
  let configured = 0;
  const saved = [];
  const nodes = Array.from({ length: 8 }, (_, i) => ({
    rawConfig: { name: String(i) },
  }));
  const collector = startIPPureCollector({
    concurrency: 2,
    intervalMs: 999999,
    store: {
      get: async () => ({ id: snapshotId, nodes }),
      saveIPPure: async (id, index) =>
        saved.push(`${id}:${index}`),
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
  // Results belong to their own immutable (snapshotId, index) pairs; the
  // queue drains across the snapshot change instead of discarding work.
  assert.equal(saved.length, nodes.length);
  assert.deepEqual(
    [...new Set(saved.map((entry) => entry.split(":")[0]))].sort(),
    ["1"],
  );
  assert.equal(configured, 2);
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

for (const transportMode of ["browser", "http"]) {
  test(`persistent signing gives exhausted ${transportMode} failures a fresh retry budget and keeps verified exit IPs`, async (t) => {
    const node = {
      rawConfig: { name: "old-browser-failure" },
      ippure: {
        status: "error",
        ip: "192.0.2.1",
        exitIPSource: "ip.sb",
        exitIPMode: "ip.sb",
        queryMode: "specified-exit",
        transportMode,
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
    assert.equal(node.ippure.signatureVersion, 2);
  });
}

test("archived browser errors and pending nodes enter collection and exhausted IPv6 errors rediscover IPv4", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-recovery-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const store = createStore();
  const file = path.join(dir, "results.json");
  await writeFile(
    file,
    JSON.stringify({
      nodes: ["legacy", "pending", "ipv6"].map((name) => ({
        name,
        rawConfig: { name, type: "http", server: `${name}.example`, port: 443 },
      })),
    }),
  );
  await store.sync(file);
  const archived = await store.get();
  await store.saveIPPure(archived.id, 0, {
    status: "error",
    transportMode: "browser",
    attempts: 3,
  });
  await store.saveIPPure(archived.id, 2, {
    status: "error",
    ip: "2001:db8::1",
    exitIPSource: "ip.sb",
    exitIPMode: "ip.sb",
    queryMode: "specified-exit",
    transportMode: "http",
    signatureVersion: 2,
    attempts: 3,
    checkedAt: new Date().toISOString(),
  });
  await writeFile(file, JSON.stringify({ nodes: [] }));
  await store.sync(file);
  assert.deepEqual(
    (await store.ippureCandidates()).map((x) => x.index),
    [0, 1, 2],
  );
  const resolutions = [];
  const collector = startIPPureCollector({
    store,
    subDir: dir,
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      resolutions.push(proxy.name);
      return "192.0.2.10";
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
  });
  t.after(async () => {
    await collector.close();
    await store.close();
  });
  await collector.run();
  assert.deepEqual(resolutions, ["legacy", "pending", "ipv6"]);
  for (const node of (await store.get(archived.id)).nodes) {
    assert.equal(node.ippure.status, "success");
    assert.equal(node.ippure.ip, "192.0.2.10");
    assert.equal(node.ippure.attempts, 1);
    assert.equal(node.ippure.exitIPVersion, 3);
  }
  assert.deepEqual(await store.ippureCandidates(), []);
});

test("failed organization backfill updates its archived snapshot only", async (t) => {
  const node = {
    rawConfig: { name: "archive" },
    ippure: {
      status: "success",
      ip: "192.0.2.1",
      exitIPSource: "ip.sb",
      score: 7,
    },
  };
  const saves = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    store: {
      get: async () => ({ id: 2, nodes: [] }),
      ippureCandidates: async () => [{ snapshotId: 1, index: 0, node }],
      saveIPPure: async (...args) => saves.push(args),
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    query: async () => {
      throw new Error("network");
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.equal(saves.length, 1);
  assert.deepEqual(saves[0].slice(0, 2), [1, 0]);
  assert.equal(saves[0][2].status, "success");
  assert.equal(saves[0][2].score, 7);
  assert.equal(saves[0][2].organizationAttempts, 1);
});

test("visible archived failures run before hidden duplicate failures and reuse archived successful transports", async (t) => {
  const donor = {
    name: "donor",
    rawConfig: { name: "donor" },
    ippure: { status: "success" },
  };
  const hidden = { name: "hidden", rawConfig: { name: "hidden" } };
  const visible = {
    name: "visible",
    rawConfig: { name: "visible" },
    snapshotId: 2,
  };
  const calls = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 3, nodes: [] }),
      aggregate: async () => ({
        nodes: [visible, { ...donor, snapshotId: 1 }],
      }),
      ippureCandidates: async () => [
        { snapshotId: 1, index: 0, node: hidden },
        { snapshotId: 2, index: 0, node: visible },
      ],
      saveIPPure: async () => {},
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      calls.push(proxy.name);
      return proxy.name === "visible" ? "192.0.2.1" : "192.0.2.2";
    },
    query: async ({ proxy, target }) => {
      assert.equal(proxy.name, "donor");
      return { ip: target, asnOrganization: "Verified" };
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(calls, ["visible", "hidden"]);
});

test("retiring earlier rows preserves enrichment indexes and never writes to retired nodes", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-retired-index-"));
  const store = createStore();
  t.after(async () => {
    await store.close();
    await rm(dir, { recursive: true, force: true });
  });
  const configs = [
    "retired-first",
    "active-middle",
    "retired-middle",
    "active-last",
  ].map((name) => ({
    name,
    type: "http",
    server: `${name}.example`,
    port: 443,
  }));
  const file = path.join(dir, "results.json");
  await writeFile(
    file,
    JSON.stringify({
      nodes: configs.map((rawConfig) => ({
        ...summarizeProxy(rawConfig),
        rawConfig,
      })),
    }),
  );
  await store.sync(file);
  const original = await store.get();
  await store.retireNodes([original.nodes[0], original.nodes[2]]);
  const calls = [];
  const collector = startIPPureCollector({
    store,
    subDir: dir,
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      calls.push(proxy.name);
      return {
        ip: proxy.name === "active-middle" ? "192.0.2.10" : "192.0.2.11",
        provider: "AWS CheckIP",
        addressFamily: 4,
      };
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(calls, ["active-middle", "active-last"]);
  const archived = await store.get(original.id);
  assert.equal(archived.nodes[0].ippure, undefined);
  assert.equal(archived.nodes[2].ippure, undefined);
  assert.equal(archived.nodes[1].ippure.ip, "192.0.2.10");
  assert.equal(archived.nodes[3].ippure.ip, "192.0.2.11");
  assert.equal(archived.nodes[1].ippure.exitIPSource, "AWS CheckIP");
  assert.deepEqual(await store.ippureCandidates(), []);
  await collector.run();
  assert.equal(calls.length, 2);
  await store.retireNodes([archived.nodes[1]]);
  await store.saveIPPure(original.id, 1, {
    status: "error",
    error: "late write",
  });
  assert.equal(
    (await store.get(original.id)).nodes[1].ippure.status,
    "success",
  );
});

test("IPv6-only nodes keep their real exit without sending unsupported addresses to risk scoring", async (t) => {
  const saves = [];
  let queries = 0;
  const collector = startIPPureCollector({
    store: {
      get: async () => ({
        id: 1,
        nodes: [{ name: "ipv6", rawConfig: { name: "ipv6" } }],
      }),
      saveIPPure: async (...args) => saves.push(args),
    },
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async () => ({
      ip: "2001:db8::23",
      provider: "AWS CheckIP",
      addressFamily: 6,
    }),
    query: async () => {
      queries++;
      throw new Error("must not query IPv6");
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.equal(queries, 0);
  assert.equal(saves.length, 1);
  assert.equal(saves[0][2].ip, "2001:db8::23");
  assert.equal(saves[0][2].status, "unsupported");
  assert.equal(saves[0][2].failureStage, "unsupported-ipv6");
});

test("a node retired during an earlier batch is removed from pending probes", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "ippure-retired-pending-"));
  const store = createStore();
  const file = path.join(dir, "results.json");
  const configs = ["first", "retire-before-probe"].map((name) => ({
    name,
    type: "http",
    server: `${name}.example`,
    port: 443,
  }));
  await writeFile(
    file,
    JSON.stringify({
      nodes: configs.map((rawConfig) => ({
        ...summarizeProxy(rawConfig),
        rawConfig,
      })),
    }),
  );
  await store.sync(file);
  const original = await store.get();
  const calls = [];
  const collector = startIPPureCollector({
    store,
    subDir: dir,
    concurrency: 1,
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      calls.push(proxy.name);
      return "192.0.2.10";
    },
    query: async ({ target }) => {
      await store.retireNodes([original.nodes[1]]);
      return { ip: target, asnOrganization: "Verified" };
    },
  });
  t.after(async () => {
    await collector.close();
    await store.close();
    await rm(dir, { recursive: true, force: true });
  });
  await collector.run();
  assert.deepEqual(calls, ["first"]);
  assert.equal((await store.get(original.id)).nodes[1].ippure, undefined);
});

test("an unavailable echo service on retry does not erase a previously observed IPv6 exit", async (t) => {
  const saves = [];
  const old = {
    status: "unsupported",
    ip: "2001:db8::23",
    exitIPMode: "proxy-echo",
    exitIPVersion: 3,
    exitIPSource: "AWS CheckIP",
    queryMode: "specified-exit",
    transportMode: "http",
    signatureVersion: 2,
    attempts: 1,
    checkedAt: "2026-10-11T00:00:00Z",
    error: "已获取 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6",
  };
  const collector = startIPPureCollector({
    store: {
      get: async () => ({
        id: 1,
        nodes: [{ rawConfig: { name: "ipv6" }, ippure: old }],
      }),
      saveIPPure: async (...args) => saves.push(args),
    },
    intervalMs: 999999,
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async () => {
      throw new Error("unreachable");
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.equal(saves[0][2].ip, old.ip);
  assert.equal(saves[0][2].status, "unsupported");
  assert.equal(saves[0][2].attempts, 2);
});

test("repeated exit-IP failures escalate to daily backoff while exhausted legacy budgets still retry", async (t) => {
  const stuck = {
    name: "stuck",
    type: "vless",
    server: "stuck.example",
    port: "443",
    rawLink:
      "vless://22345678-1234-1234-1234-123456789abc@stuck.example:443",
    ippure: {
      status: "error",
      failureStage: "exit-ip",
      attempts: 3,
      checkedAt: new Date().toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: 3,
      exitIPProbeVersion: 1,
    },
  };
  const fresh = {
    name: "fresh",
    type: "vless",
    server: "fresh.example",
    port: "443",
    rawLink:
      "vless://32345678-1234-1234-1234-123456789abc@fresh.example:443",
  };
  const donor = {
    name: "donor",
    rawConfig: { name: "donor" },
    ippure: {
      status: "success",
      ip: "192.0.2.10",
      exitIPMode: "proxy-echo",
      exitIPVersion: 3,
      exitIPSource: "AWS CheckIP",
      asnOrganization: "Verified",
    },
  };
  const nodes = [stuck, fresh, donor],
    calls = [],
    saves = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (id, index, result) => {
        saves.push([id, index, result]);
        nodes[index].ippure = result;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      calls.push(proxy.name);
      return { ip: "192.0.2.11", provider: "AWS CheckIP", addressFamily: 4 };
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
  });
  t.after(() => collector.close());
  // Three recent exit-IP failures mean the node waits a full day; the
  // never-checked node is not starved behind it.
  await collector.run();
  assert.deepEqual(calls, ["fresh"]);
  assert.equal(nodes[0].ippure.attempts, 3);
  // Once the day has passed the same budget buys exactly one more probe.
  stuck.ippure = {
    ...stuck.ippure,
    checkedAt: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
  };
  await collector.run();
  assert.deepEqual(calls, ["fresh", "stuck"]);
  assert.equal(saves.at(-1)[2].status, "success");
  assert.equal(saves.at(-1)[2].attempts, 4);
  await collector.run();
  assert.equal(calls.length, 2);
});

test("fourth exit-IP failure retires the node when the run has a healthy control", async (t) => {
  const stuck = {
    name: "stuck",
    type: "vless",
    server: "stuck.example",
    port: "443",
    rawLink:
      "vless://42345678-1234-1234-1234-123456789abc@stuck.example:443",
    ippure: {
      status: "error",
      failureStage: "exit-ip",
      attempts: 3,
      checkedAt: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: 3,
      exitIPProbeVersion: 1,
    },
  };
  const fresh = {
    name: "fresh",
    type: "vless",
    server: "fresh.example",
    port: "443",
    rawLink:
      "vless://52345678-1234-1234-1234-123456789abc@fresh.example:443",
  };
  const nodes = [stuck, fresh],
    calls = [],
    retirements = [];
  const retiredSet = new Set();
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (id, index, result) => {
        nodes[index].ippure = result;
      },
      retiredNodeIdentities: async () => retiredSet,
      retireNodes: async (items, reason) => {
        for (const node of items) {
          retirements.push([node.name, reason]);
          retiredSet.add(aggregateIdentity(node));
        }
        return items.length;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      calls.push(proxy.name);
      if (proxy.name === "stuck") throw new Error("connection reset");
      return { ip: "192.0.2.11", provider: "AWS CheckIP", addressFamily: 4 };
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
    githubProbe: async () => false,
  });
  t.after(() => collector.close());
  // The never-checked node succeeds first and proves the transport healthy;
  // the fourth exit-IP failure of the dead node then retires it.
  await collector.run();
  assert.deepEqual(calls, ["fresh", "stuck"]);
  assert.deepEqual(retirements, [
    ["stuck", "IPPure: exit-IP probe failed 4 times"],
  ]);
  // Retired identities leave the collection queue entirely.
  await collector.run();
  assert.deepEqual(calls, ["fresh", "stuck"]);
});

test("a fourth exit-IP failure keeps a GitHub-reachable node in the fleet", async (t) => {
  const stuck = {
    name: "stuck",
    type: "vless",
    server: "stuck.example",
    port: "443",
    rawLink:
      "vless://42345678-1234-1234-1234-123456789abc@stuck.example:443",
    ippure: {
      status: "error",
      failureStage: "exit-ip",
      attempts: 3,
      checkedAt: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: 3,
      exitIPProbeVersion: 1,
    },
  };
  const fresh = {
    name: "fresh",
    type: "vless",
    server: "fresh.example",
    port: "443",
    rawLink:
      "vless://52345678-1234-1234-1234-123456789abc@fresh.example:443",
  };
  const nodes = [stuck, fresh],
    retirements = [],
    probes = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (id, index, result) => {
        nodes[index].ippure = result;
      },
      retiredNodeIdentities: async () => new Set(),
      retireNodes: async (items, reason) => {
        retirements.push([items[0].name, reason]);
        return items.length;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, `http://localhost:7891/${p.name}`])),
    resolveIP: async ({ proxy }) => {
      if (proxy.name === "stuck") throw new Error("connection reset");
      return { ip: "192.0.2.11", provider: "AWS CheckIP", addressFamily: 4 };
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
    // Echo endpoints are blocked, but the node's own tunnel reaches GitHub.
    githubProbe: async ({ proxyUrl }) => {
      probes.push(proxyUrl);
      return true;
    },
  });
  t.after(() => collector.close());
  await collector.run();
  assert.deepEqual(retirements, []);
  assert.equal(nodes[0].ippure.githubReachable, true);
  assert.equal(nodes[0].ippure.attempts, 4);
  assert.equal(nodes[0].ippure.failureStage, "exit-ip");
  // The corroboration probe dials the stuck node's own isolated listener.
  assert.equal(probes.length, 1);
  assert.ok(probes[0].includes("stuck"));
});

test("exit-IP failures never retire when the whole run is unhealthy", async (t) => {
  const stuck = {
    name: "stuck",
    type: "vless",
    server: "stuck.example",
    port: "443",
    rawLink:
      "vless://62345678-1234-1234-1234-123456789abc@stuck.example:443",
    ippure: {
      status: "error",
      failureStage: "exit-ip",
      attempts: 3,
      checkedAt: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: 3,
      exitIPProbeVersion: 1,
    },
  };
  const nodes = [stuck],
    retirements = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (id, index, result) => {
        nodes[index].ippure = result;
      },
      retireNodes: async (items, reason) => {
        retirements.push([items[0].name, reason]);
        return items.length;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async () => {
      throw new Error("connection reset");
    },
  });
  t.after(() => collector.close());
  // Without a successful control in the run the failure may be an outage;
  // retirement is deferred to a future healthy run.
  await collector.run();
  assert.deepEqual(retirements, []);
  assert.equal(nodes[0].ippure.attempts, 4);
});

test("success results verified before country collection gain it via one trace request", async (t) => {
  const verified = {
    name: "verified",
    type: "vless",
    server: "verified.example",
    port: "443",
    rawLink:
      "vless://72345678-1234-1234-1234-123456789abc@verified.example:443",
    ippure: {
      status: "success",
      ip: "192.0.2.10",
      attempts: 1,
      checkedAt: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: 3,
      exitIPProbeVersion: 1,
      asnOrganization: "Verified",
    },
  };
  const nodes = [verified],
    countryCalls = [],
    probeCalls = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 1,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (id, index, result) => {
        nodes[index].ippure = result;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      probeCalls.push(proxy.name);
      return { ip: "192.0.2.11", provider: "AWS CheckIP", addressFamily: 4 };
    },
    resolveCountry: async () => {
      countryCalls.push("192.0.2.10");
      return "DE";
    },
    query: async ({ target }) => ({ ip: target, asnOrganization: "Verified" }),
  });
  t.after(() => collector.close());
  // The verified exit IP is reused without a fresh probe; only the trace
  // country lookup runs, riding the node's own transport.
  await collector.run();
  assert.deepEqual(probeCalls, []);
  assert.deepEqual(countryCalls, ["192.0.2.10"]);
  assert.equal(nodes[0].ippure.country, "DE");
  assert.equal(nodes[0].ippure.ip, "192.0.2.10");
  // With the country stored the result is final and never scheduled again.
  await collector.run();
  assert.equal(countryCalls.length, 1);
});

test("exit-IP failure backoff escalates through 30 minutes, 6 hours and 24 hours", async (t) => {
  const makeNode = (name, attempts) => ({
    name,
    rawConfig: { name, type: "http", server: `${name}.example`, port: 443 },
    ippure: {
      status: "error",
      failureStage: "exit-ip",
      attempts,
      checkedAt: new Date().toISOString(),
      queryMode: "specified-exit",
      exitIPMode: "proxy-echo",
      transportMode: "http",
      signatureVersion: 2,
      exitIPVersion: EXIT_IP_VERSION,
      exitIPProbeVersion: EXIT_IP_PROBE_VERSION,
    },
  });
  const nodes = [makeNode("a1", 1), makeNode("a2", 2), makeNode("a3", 3)];
  const calls = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 3,
    store: {
      get: async () => ({ id: 1, nodes }),
      aggregate: async () => ({ nodes }),
      saveIPPure: async (_id, index, result) => {
        nodes[index].ippure = result;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => {
      calls.push(proxy.name);
      return { ip: "192.0.2.11", provider: "AWS CheckIP", addressFamily: 4 };
    },
    query: async ({ target }) => ({
      ip: target,
      asnOrganization: "Verified",
      country: "DE",
    }),
  });
  t.after(() => collector.close());
  const age = (ms) => {
    for (const node of nodes)
      node.ippure.checkedAt = new Date(Date.now() - ms).toISOString();
  };
  // Recent failures of any attempt count stay inside their backoff window.
  await collector.run();
  assert.deepEqual(calls, []);
  // Past 30 minutes only the first failure retries; 6h/24h still hold.
  age(31 * 60 * 1000);
  await collector.run();
  assert.deepEqual(calls, ["a1"]);
  // Past 6 hours the second failure retries; 24h still holds.
  age(7 * 60 * 60 * 1000);
  await collector.run();
  assert.deepEqual(calls, ["a1", "a2"]);
  // Past 24 hours the third failure gets its final scheduled probe.
  age(25 * 60 * 60 * 1000);
  await collector.run();
  assert.deepEqual(calls, ["a1", "a2", "a3"]);
});

test("a slow job inside a wave does not block other jobs from finishing", async (t) => {
  const nodes = ["slow", "fast1", "fast2", "fast3"].map((name) => ({
    name,
    rawConfig: { name, type: "http", server: `${name}.example`, port: 443 },
  }));
  const targets = { slow: "192.0.2.9", fast1: "192.0.2.1", fast2: "192.0.2.2", fast3: "192.0.2.3" };
  const saved = [];
  const collector = startIPPureCollector({
    intervalMs: 999999,
    concurrency: 2,
    store: {
      get: async () => ({ id: 1, nodes }),
      saveIPPure: async (_id, index, result) => {
        saved.push(nodes[index].name);
        nodes[index].ippure = result;
      },
    },
    configure: async ({ proxies }) =>
      new Map(proxies.map((p) => [p, "http://localhost:7891"])),
    resolveIP: async ({ proxy }) => targets[proxy.name],
    query: async ({ target }) => {
      // The stalled job occupies one window slot; the other slot keeps
      // draining the remaining wave jobs meanwhile.
      if (target === targets.slow)
        await new Promise((resolve) => setTimeout(resolve, 200));
      return { ip: target, asnOrganization: "Verified", country: "DE" };
    },
  });
  t.after(() => collector.close());
  await collector.run();
  // All four jobs share one wave; the fast jobs finish and persist while the
  // slow job is still stalled, which a batch barrier would have prevented.
  assert.deepEqual(saved, ["fast1", "fast2", "fast3", "slow"]);
  assert.equal(
    nodes.filter((node) => node.ippure.status === "success").length,
    4,
  );
  assert.equal(collector.status().error, null);
});
