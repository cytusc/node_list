import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import pg from "pg";
import { createPostgresStore } from "./postgres-store.js";
import { createApp } from "./app.js";

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
    t.after(async () => {
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
    assert.equal(old.nodes[0].name, "Archived");
    const app = createApp({ subDir: dir, resultsFile: file, store: reopened });
    const server = app.listen(0, "127.0.0.1");
    await new Promise((resolve) => server.once("listening", resolve));
    t.after(async () => {
      await new Promise((resolve) => server.close(resolve));
      await app.locals.close();
    });
    const get = (url) =>
      fetch(`http://127.0.0.1:${server.address().port}${url}`);
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
    const history = await (await get("/api/history?limit=1&offset=1")).json();
    assert.equal(history.total, 2);
    assert.equal(history.items[0].id, old.id);
    assert.equal(history.items[0].nodeCount, 1);
    await rm(file);
    const saved = await (await get(`/api/results?snapshot=${old.id}`)).json();
    assert.equal(saved.nodes[0].name, "Archived");
    assert.equal(saved.sourceWarning, "NO_RESULTS");
  },
);
