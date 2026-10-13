import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { summarizeProxy, aggregateIdentity } from "./node-sources.js";

test("retired nodes stay hidden from current and aggregate views and IP checks", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "retired-nodes-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const proxy = {
    name: "unreachable",
    type: "http",
    server: "unreachable.example",
    port: 443,
  };
  const file = path.join(dir, "results.json");
  const store = createStore(path.join(dir, "store.sqlite"));
  t.after(() => store.close());
  await writeFile(
    file,
    JSON.stringify({ nodes: [{ ...summarizeProxy(proxy), rawConfig: proxy }] }),
  );
  await store.sync(file);
  const archived = await store.get();
  assert.equal(archived.nodes.length, 1);

  assert.equal(
    await store.retireNodes(archived.nodes, "GitHub unavailable"),
    1,
  );
  assert.equal((await store.get()).nodes.length, 0);
  assert.equal((await store.get(archived.id)).nodes.length, 1);
  assert.equal((await store.aggregate()).nodes.length, 0);
  assert.deepEqual(await store.ippureCandidates(), []);

  await writeFile(
    file,
    JSON.stringify({
      checkedAt: "2026-10-12T00:00:00Z",
      nodes: [{ ...summarizeProxy(proxy), rawConfig: proxy, speed: 100 }],
    }),
  );
  await store.sync(file);
  assert.equal((await store.get()).nodes.length, 0);
  assert.equal((await store.aggregate()).nodes.length, 0);
});

test("ambiguous legacy results written after retirement are rediscovered without deleting raw records", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "retired-legacy-results-"));
  const store = createStore();
  t.after(async () => {
    await store.close();
    await rm(dir, { recursive: true, force: true });
  });
  const file = path.join(dir, "results.json");
  const configs = ["before", "retired", "after"].map((name) => ({
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
  await store.retireNodes([original.nodes[1]]);
  const old = {
    status: "success",
    ip: "8.8.8.8",
    exitIPSource: "ip.sb",
    exitIPVersion: 2,
    asnOrganization: "Old",
    checkedAt: new Date(Date.now() + 1000).toISOString(),
  };
  await store.saveIPPure(original.id, 0, old);
  await store.saveIPPure(original.id, 2, old);
  const visible = await store.get();
  assert.equal(visible.nodes[0].ippure.ip, "8.8.8.8");
  assert.equal(visible.nodes[1].ippure, undefined);
  assert.equal((await store.aggregate()).nodes[1].ippure, undefined);
  assert.deepEqual(
    (await store.ippureCandidates()).map((x) => x.index),
    [2],
  );
  await store.saveIPPure(original.id, 2, {
    ...old,
    ip: "192.0.2.23",
    exitIPMode: "proxy-echo",
    exitIPVersion: 3,
  });
  assert.equal((await store.get()).nodes[1].ippure.ip, "192.0.2.23");
  assert.deepEqual(await store.ippureCandidates(), []);
});

test("restoration makes archived nodes visible and protects manual or newer retirements", async (t) => {
  const store = createStore();
  t.after(() => store.close());
  const rawConfig = {
    name: "recover",
    type: "http",
    server: "recover.example",
    port: 443,
  };
  await store.syncSnapshot({
    nodes: [{ ...summarizeProxy(rawConfig), rawConfig }],
  });
  const snapshot = await store.get();
  const identity = aggregateIdentity(snapshot.nodes[0]);
  const reason =
    "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed";
  await store.retireNodes(snapshot.nodes, reason);
  const row = (await store.retiredNodes())[0];
  assert.equal((await store.aggregate()).nodes.length, 0);
  assert.equal(
    (await store.aggregate({ includeRetired: true })).nodes.length,
    1,
  );
  assert.equal(await store.restoreNode(identity, "stale-retirement"), 0);
  assert.equal(await store.restoreNode(identity, row.retiredAt), 1);
  assert.equal((await store.aggregate()).nodes.length, 1);
  assert.equal((await store.getCollectionSnapshot()).nodes.length, 1);
  assert.equal((await store.ippureCandidates()).length, 1);
  await store.retireNodes(snapshot.nodes, "Manually disabled");
  const manual = (await store.retiredNodes())[0];
  assert.equal(await store.restoreNode(identity, manual.retiredAt), 0);
  assert.equal((await store.aggregate()).nodes.length, 0);
});
