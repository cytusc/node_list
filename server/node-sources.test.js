import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { stringify } from "yaml";
import pg from "pg";
import { createStore } from "./store.js";
import { createPostgresStore } from "./postgres-store.js";
import {
  summarizeProxy,
  sourceKey,
  readNodeSources,
  sourceForNode,
} from "./node-sources.js";

const proxy = {
  name: "Keep",
  type: "ss",
  server: "node.example",
  port: 443,
  cipher: "aes-256-gcm",
  password: "secret",
};
const node = summarizeProxy(proxy);

for (const backend of ["sqlite", "postgres"]) {
  test(
    `${backend}: archives full configs, explicitly prunes history and preserves enrichment`,
    { skip: backend === "postgres" && !process.env.TEST_POSTGRES_URL },
    async (t) => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "node-source-test-"));
      t.after(() => rm(dir, { recursive: true, force: true }));
      const file = path.join(dir, "results.json");
      const clashFile = path.join(dir, "all.yaml");
      const linksFile = path.join(dir, "base64.txt");
      let config;
      if (backend === "postgres") {
        const admin = new pg.Pool({
          connectionString: process.env.TEST_POSTGRES_URL,
        });
        const schema = `node_sources_test_${process.pid}_${Date.now()}`;
        await admin.query(`CREATE SCHEMA "${schema}"`);
        t.after(async () => {
          await admin.query(`DROP SCHEMA "${schema}" CASCADE`);
          await admin.end();
        });
        config = {
          connectionString: process.env.TEST_POSTGRES_URL,
          options: `-c search_path=${schema}`,
        };
      }
      const open = () =>
        backend === "sqlite"
          ? createStore(path.join(dir, "history.sqlite"))
          : createPostgresStore(config);
      let store = await open();
      t.after(() => store.close());
      const write = (checkedAt, nodes) =>
        writeFile(file, JSON.stringify({ checkedAt, speedTest: true, nodes }));
      const unknown = { ...node, name: "Lost", server: "lost.example" };
      await write("2026-10-10T00:00:00Z", [unknown, node]);
      await store.sync(file);
      const first = await store.get();
      await store.saveIPPure(first.id, 0, {
        status: "success",
        ip: "192.0.2.1",
      });
      await store.saveIPPure(first.id, 1, {
        status: "success",
        ip: "192.0.2.2",
      });
      // Same detection time, different input: migration must not create a hash
      // collision when both batches become the same payload after pruning.
      await write("2026-10-10T00:00:00Z", [node]);
      await store.sync(file);
      const latest = await store.get();
      await store.saveIPPure(latest.id, 0, {
        status: "success",
        ip: "192.0.2.3",
      });
      await writeFile(clashFile, stringify({ proxies: [proxy] }));
      const sources = { clashFile, linksFile }; // missing share file is valid
      await store.sync(file, sources);
      assert.equal((await store.get()).id, latest.id);
      assert.equal((await store.history(10, 0)).total, 2);
      assert.equal((await store.get(first.id)).nodes.length, 2); // startup does not delete
      assert.deepEqual((await store.get()).nodes[0].rawConfig, proxy);
      assert.equal((await store.get()).nodes[0].rawLink, undefined);
      await store.reconcileSources(
        new Map([[sourceKey(node), { rawConfig: proxy }]]),
        { prune: true },
      );
      const saved = await store.get(first.id);
      assert.equal(saved.nodes.length, 1);
      assert.equal(saved.nodes[0].ippure.ip, "192.0.2.2");
      assert.equal(
        (await store.get(latest.id)).nodes[0].ippure.ip,
        "192.0.2.3",
      );
      assert.equal((await store.history(10, 0)).total, 2);

      await store.close();
      store = await open();
      await store.sync(file, sources);
      assert.equal((await store.history(10, 0)).total, 2);
      // Results and output files can be published at different times. Fail the
      // whole import until matching source data exists; keep the previous batch.
      await write("2026-10-11T00:00:00Z", [node, unknown]);
      await assert.rejects(store.sync(file, sources));
      assert.equal((await store.get()).id, latest.id);
      await write("2026-10-11T00:00:00Z", [node]);
      const link = "ss://aes-256-gcm:secret@node.example:443#Keep";
      await writeFile(linksFile, Buffer.from(link).toString("base64"));
      await store.sync(file, sources);
      assert.equal((await store.get()).nodes[0].rawLink, link);
      await store.close();
      store = await open();
      await writeFile(
        clashFile,
        stringify({ proxies: [{ ...proxy, password: "new-secret" }] }),
      );
      await write("2026-10-12T00:00:00Z", [node]);
      await store.sync(file, sources);
      assert.equal(
        (await store.get(first.id)).nodes[0].rawConfig.password,
        "secret",
      );
      assert.equal(
        (await store.get()).nodes[0].rawConfig.password,
        "new-secret",
      );
      assert.equal((await store.get()).nodes[0].rawLink, undefined); // stale link rejected
      assert.equal((await store.aggregate()).nodes.length, 2); // distinct credentials
    },
  );
}

test("reused names cannot attach another endpoint's link; shared endpoints retain separate credentials", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "node-source-match-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const clashFile = path.join(dir, "all.yaml");
  const linksFile = path.join(dir, "base64.txt");
  await writeFile(
    clashFile,
    stringify({
      proxies: [proxy, { ...proxy, name: "Second", password: "second-secret" }],
    }),
  );
  await writeFile(linksFile, "ss://aes-256-gcm:secret@other.example:443#Keep");
  const sources = await readNodeSources({ clashFile, linksFile });
  assert.equal(sourceForNode(node, sources), null); // ambiguous archived summary
  assert.equal(
    sourceForNode(node, sources, { exactNames: true }).rawConfig.password,
    "secret",
  );
  assert.equal(
    sourceForNode(node, sources, { exactNames: true }).rawLink,
    undefined,
  );
  assert.equal(
    sourceForNode({ ...node, name: "Second" }, sources, { exactNames: true })
      .rawConfig.password,
    "second-secret",
  );
});
