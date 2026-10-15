import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import pg from "pg";
import { createPostgresStore } from "./postgres-store.js";
import { createApp } from "./app.js";
import { summarizeProxy } from "./node-sources.js";

test(
  "PostgreSQL persists batches, handles concurrent duplicates and serves history APIs",
  {
    skip: !process.env.TEST_POSTGRES_URL,
  },
  async (t) => {
    const admin = new pg.Pool({
      connectionString: process.env.TEST_POSTGRES_URL,
    });
    const schema = `node_explorer_test_${process.pid}_${Date.now()}`;
    await admin.query(`CREATE SCHEMA "${schema}"`);
    let app, server;
    t.after(async () => {
      if (server) await new Promise((resolve) => server.close(resolve));
      await app?.locals.close();
      await admin.query(`DROP SCHEMA "${schema}" CASCADE`);
      await admin.end();
    });
    const config = {
      connectionString: process.env.TEST_POSTGRES_URL,
      options: `-c search_path=${schema}`,
    };
    const dir = await mkdtemp(path.join(os.tmpdir(), "node-pg-test-"));
    t.after(() => rm(dir, { recursive: true, force: true }));
    const file = path.join(dir, "results.json");
    const first = {
      checkedAt: "2026-10-11T00:00:00Z",
      speedTest: true,
      nodes: [{ name: "Archived", password: "private" }],
    };
    await writeFile(file, JSON.stringify(first));
    const store = await createPostgresStore(config);
    const peer = await createPostgresStore(config);
    try {
      await Promise.all([store.sync(file), peer.sync(file)]);
      assert.equal((await store.history(10, 0)).total, 1);
      assert.equal("password" in (await store.get()).nodes[0], false);
    } finally {
      await store.close();
      await peer.close();
    }
    const reopened = await createPostgresStore(config);
    const old = await reopened.get();
    const ippure = {
      status: "success",
      score: 32,
      label: "中性",
      checkedAt: "2026-10-11T00:00:00Z",
      attempts: 1,
    };
    await reopened.saveIPPure(old.id, 0, ippure);
    assert.deepEqual((await reopened.get(old.id)).nodes[0].ippure, ippure);
    assert.equal(old.nodes[0].name, "Archived");
    app = createApp({ subDir: dir, resultsFile: file, store: reopened });
    server = app.listen(0, "127.0.0.1");
    await new Promise((resolve) => server.once("listening", resolve));
    const get = (url) =>
      fetch(`http://127.0.0.1:${server.address().port}${url}`);
    // createApp starts an initial background import. Join it before replacing
    // the file, otherwise sync() may correctly coalesce the request with the
    // still-running import and the API will observe the previous snapshot.
    await reopened.sync(file);
    await writeFile(
      file,
      JSON.stringify({
        ...first,
        checkedAt: "2026-10-11T02:00:00Z",
        nodes: [],
      }),
    );
    const latest = await (await get("/api/results")).json();
    assert.notEqual(latest.id, old.id);
    assert.equal(await reopened.latestSnapshotId(), latest.id);
    const context = await reopened.getCollectionContext();
    assert.equal(context.snapshot.id, latest.id);
    assert.deepEqual(context.historical, await reopened.ippureCandidates());
    assert.ok(Array.isArray(context.aggregate.nodes));
    const history = await (await get("/api/history?limit=1&offset=1")).json();
    assert.equal(history.total, 2);
    assert.equal(history.items[0].id, old.id);
    assert.equal(history.items[0].nodeCount, 1);
    const aggregated = await (await get("/api/results?view=aggregate")).json();
    assert.equal(aggregated.batchCount, 2);
    assert.equal(aggregated.nodes[0].name, "Archived");
    assert.equal(aggregated.nodes[0].snapshotId, old.id);
    assert.deepEqual(aggregated.nodes[0].ippure, ippure);
    await rm(file);
    const saved = await (await get(`/api/results?snapshot=${old.id}`)).json();
    assert.equal(saved.nodes[0].name, "Archived");
    assert.equal(saved.sourceWarning, "NO_RESULTS");
  },
);

test(
  "PostgreSQL keeps original collection indexes and blocks retired enrichment writes",
  { skip: !process.env.TEST_POSTGRES_URL },
  async (t) => {
    const admin = new pg.Pool({
      connectionString: process.env.TEST_POSTGRES_URL,
    });
    const schema = `node_explorer_retired_test_${process.pid}_${Date.now()}`;
    await admin.query(`CREATE SCHEMA "${schema}"`);
    const store = await createPostgresStore({
      connectionString: process.env.TEST_POSTGRES_URL,
      options: `-c search_path=${schema}`,
    });
    const dir = await mkdtemp(path.join(os.tmpdir(), "node-pg-retired-"));
    t.after(async () => {
      await store.close();
      await admin.query(`DROP SCHEMA "${schema}" CASCADE`);
      await admin.end();
      await rm(dir, { recursive: true, force: true });
    });
    const file = path.join(dir, "results.json");
    const configs = ["retired", "active"].map((name) => ({
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
    await store.retireNodes([original.nodes[0]]);
    const snapshot = await store.getCollectionSnapshot();
    assert.deepEqual(snapshot.nodeIndexes, [1]);
    await store.saveIPPure(original.id, 1, {
      status: "success",
      ip: "192.0.2.23",
    });
    await store.saveIPPure(original.id, 0, { status: "error" });
    const archived = await store.get(original.id);
    assert.equal(archived.nodes[0].ippure, undefined);
    assert.equal(archived.nodes[1].ippure.ip, "192.0.2.23");
    assert.equal((await store.get()).nodes.length, 1);
  },
);
