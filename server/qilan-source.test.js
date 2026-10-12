import { test } from "node:test";
import assert from "node:assert/strict";
import { fetchQilanSnapshot } from "./qilan-source.js";
import { createStore } from "./store.js";

const item = (id) => ({
  id, name: `Node ${id}`, protocol: "vless", server: "example.com",
  port: 443, region: "DE", network: "ws", status: "online", ping: 12,
  last_check: "2026-10-12T06:00:00Z",
  link: `vless://credential-${id}@example.com:443?security=tls&sni=example.com#Node`,
});

test("Qilan paginates authenticated requests and archives recoverable nodes", async () => {
  const pages = [];
  const snapshot = await fetchQilanSnapshot({
    session: "test-cookie",
    sleep: async () => {},
    fetchImpl: async (url, options) => {
      assert.equal(options.headers.Cookie, "session=test-cookie");
      pages.push(Number(url.searchParams.get("page")));
      return { ok: true, status: 200, json: async () => ({
        total: 201,
        data: pages.length === 1 ? Array.from({ length: 200 }, (_, i) => item(i + 1)) : [item(201)],
      }) };
    },
  });
  assert.deepEqual(pages, [1, 2]);
  assert.equal(snapshot.nodes.length, 201);
  assert.equal(snapshot.nodes[0].subTag, "Qilan");
  assert.equal(snapshot.nodes[0].tls, true);
  const store = createStore();
  try {
    await Promise.all([store.syncSnapshot(snapshot), store.syncSnapshot(snapshot)]);
    assert.equal((await store.history(10, 0)).total, 1);
    assert.equal((await store.aggregate()).nodes.length, 201);
    assert.equal((await store.get()).nodes[0].rawLink, item(1).link);
  } finally {
    await store.close();
  }
});

test("Qilan rejects a failed later page without publishing a partial snapshot", async () => {
  let requests = 0;
  await assert.rejects(fetchQilanSnapshot({
    session: "test-cookie", sleep: async () => {},
    fetchImpl: async () => ++requests === 1
      ? { ok: true, status: 200, json: async () => ({ total: 201, data: Array.from({ length: 200 }, (_, i) => item(i + 1)) }) }
      : { ok: false, status: 403 },
  }), /HTTP 403/);
  assert.equal(requests, 2);
});
