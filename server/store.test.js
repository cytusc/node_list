import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createStore } from "./store.js";
import { summarizeProxy } from "./node-sources.js";

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
