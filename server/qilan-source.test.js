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

test("Qilan deduplicates connections across pages and keeps the newest check", async () => {
  const older = item(1);
  const newer = {
    ...older, id: 999, name: "Renamed", ping: 23,
    last_check: "2026-10-12T07:00:00Z",
    link: older.link.replace("#Node", "#Renamed"),
  };
  const snapshot = await fetchQilanSnapshot({
    session: "test-cookie", sleep: async () => {},
    fetchImpl: async (url) => ({ ok: true, status: 200, json: async () => ({
      total: 202,
      data: url.searchParams.get("page") === "1"
        ? Array.from({ length: 200 }, (_, i) => item(i + 1))
        : [newer, { ...older, id: 1000, ping: 99 }],
    }) }),
  });
  assert.equal(snapshot.nodes.length, 200);
  assert.equal(snapshot.nodes[0].baseName, "Renamed");
  assert.equal(snapshot.nodes[0].details.find(({ k }) => k === "latency").v, "23 ms");
  assert.equal(snapshot.checkedAt, newer.last_check.replace("Z", ".000Z"));
});

test("existing Qilan history merges changing metadata but retains distinct connections", async () => {
  // Deliberately bypass fetch-time deduplication to reproduce already archived duplicates.
  const store = createStore();
  const common = {
    name: "AE 04 [Qilan]", baseName: "AE 04", subTag: "Qilan",
    type: "vless", server: "example.com", port: "443", network: "ws",
    rawLink: "vless://credential@example.com:443?security=tls&type=ws&path=%2Fws#Old",
  };
  try {
    await store.syncSnapshot({ checkedAt: "2026-10-12T06:00:00Z", nodes: [
      { ...common, details: [{ k: "source-id", v: "1" }, { k: "latency", v: "10 ms" }] },
      { ...common, details: [{ k: "source-id", v: "2" }] },
      { ...common, rawLink: common.rawLink.replace("credential@", "other@") },
      { ...common, rawLink: common.rawLink.replace("%2Fws", "%2Fother") },
    ] });
    await store.syncSnapshot({ checkedAt: "2026-10-12T07:00:00Z", nodes: [{
      ...common, name: "Renamed", speed: 1024,
      rawLink: "vless://credential@example.com:443?path=%2Fws&type=ws&security=tls#New",
      details: [{ k: "source-id", v: "999" }, { k: "latency", v: "20 ms" }, { k: "status", v: "online" }],
    }] });
    const aggregate = await store.aggregate();
    assert.equal(aggregate.nodes.length, 3);
    assert.equal(aggregate.nodes[0].name, "Renamed");
    assert.equal(aggregate.nodes[0].speed, 1024);
    assert.equal(aggregate.nodes[0].checkedAt, "2026-10-12T07:00:00Z");
    assert.equal((await store.history(10, 0)).total, 2);
  } finally {
    await store.close();
  }
});
