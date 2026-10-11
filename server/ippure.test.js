import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { startIPPureCollector } from "./ippure.js";

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
  const collector = startIPPureCollector({
    store,
    subDir: dir,
    intervalMs: 999999,
    query: async ({ proxy }) => {
      calls.push(proxy.name);
      assert.equal(proxy.password, "private");
      if (proxy.name === "bad" || failBackfill)
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
