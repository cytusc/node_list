import { test } from "node:test";
import assert from "node:assert/strict";
import pg from "pg";
import { createPostgresStore } from "./postgres-store.js";
import { aggregateIdentity } from "./node-sources.js";
import {
  filterDepartedNodes,
  pruneOldSnapshots,
  refreshResultsProjection,
} from "./postgres-results.js";
import {
  paginateResults,
  parseResultsQuery,
  resultSortKeys,
} from "./results-query.js";

const enabled = { skip: !process.env.TEST_POSTGRES_URL };
const json = (value) => JSON.parse(JSON.stringify(value));
async function fixture(t) {
  const admin = new pg.Pool({
    connectionString: process.env.TEST_POSTGRES_URL,
  });
  const schema = `results_test_${process.pid}_${Date.now()}`;
  await admin.query(`CREATE SCHEMA "${schema}"`);
  const stores = [];
  t.after(async () => {
    for (const store of stores) await store.close();
    await admin.query(`DROP SCHEMA "${schema}" CASCADE`);
    await admin.end();
  });
  const config = {
    connectionString: process.env.TEST_POSTGRES_URL,
    options: `-c search_path=${schema}`,
  };
  const open = async () => {
    const store = await createPostgresStore(config);
    stores.push(store);
    return store;
  };
  const store = await open();
  // Keep a direct source reader as the independent oracle; the optimized
  // public aggregate reader itself uses the projection.
  const reference = await createPostgresStore(config, {
    maintenanceWorker: true,
  });
  stores.push(reference);
  return { store, reference, open, admin, schema, config };
}

test(
  "SQL pages match aggregate filtering, natural Chinese sorting, null placement and global statistics",
  enabled,
  async (t) => {
    const { store, reference } = await fixture(t);
    const names = ["🇯🇵 节点 10", "节点 2", "香港01", "香港1", "Alpha", "Alpha"];
    const nodes = Array.from({ length: 127 }, (_, i) => ({
      name: names[i % names.length],
      server: `node-${i}.example`,
      port: 443,
      type: i % 3 ? "vless" : "http",
      country: i % 3 ? "JP" : undefined,
      network: i % 2 ? "ws" : "tcp",
      tls: i % 2 === 0,
      reality: i % 3 === 0,
      udp: i % 4 === 0,
      speed: i % 4 ? (i % 11) * 1024 : undefined,
      media: [
        { Platform: "openai", Tag: i % 2 ? "US" : "" },
        { platform: "netflix", tag: i % 3 ? "JP" : "" },
      ],
      rawConfig: {
        name: names[i % names.length],
        type: "http",
        server: `node-${i}.example`,
        port: 443,
        password: "fixture",
      },
    }));
    await store.syncSnapshot({
      checkedAt: "2026-10-11T00:00:00Z",
      speedTest: true,
      mediaCheck: true,
      nodes,
    });
    const first = await store.get();
    for (let i = 0; i < 21; i++) {
      await store.saveIPPure(first.id, i, {
        status: i % 5 ? "success" : "error",
        score: i % 4 ? i : 0,
        asnOrganization: i % 2 ? "网络 10" : "网络 2",
        source: i % 3 ? "ISP" : undefined,
        attributes: i % 2 ? ["住宅", "IPv4"] : [],
        humanPercent: i % 4 ? 20 : 0,
      });
    }
    await store.syncSnapshot({
      checkedAt: "2026-10-12T00:00:00Z",
      nodes: [{ ...nodes[4], name: "Renamed", speed: 0 }],
    });
    // Import order must not let an old detection supersede a newer one.
    await store.syncSnapshot({
      checkedAt: "2026-10-09T00:00:00Z",
      nodes: [{ ...nodes[4], name: "Old", speed: 99999 }],
    });
    await store.refreshAggregate();
    const snapshot = await reference.aggregate();
    for (const sort of resultSortKeys)
      for (const direction of ["asc", "desc"]) {
        for (const page of ["1", "2", "99"]) {
          const query = parseResultsQuery({
            page,
            limit: "7",
            sort,
            direction,
          });
          assert.deepEqual(
            json(await store.aggregatePage(query)),
            json(paginateResults(snapshot, query)),
            `${sort}/${direction}/${page}`,
          );
        }
      }
    for (const filters of [
      {},
      { country: "unknown" },
      { country: "JP", type: "vless" },
      { network: "ws" },
      { ipSource: "ISP" },
      { ipSource: "ISP,missing" },
      { ipAttributes: "住宅" },
      { ipAttributes: "IPv4,missing" },
      { ipSource: "ISP", ipAttributes: "住宅", country: "JP" },
      { ipSource: "missing" },
      { ipAttributes: "missing" },
      { ipSource: "' OR 1=1 --" },
      { features: "tls,udp" },
      { features: "reality" },
      { media: "openai,netflix" },
      { minSpeed: "5" },
      { country: "JP", features: "tls", media: "netflix", minSpeed: "3" },
      { q: "日本" },
      { q: "node-12.example" },
      { q: "未知地区" },
      { q: "_%" },
      { q: "' OR 1=1 --" },
      { q: "no-match" },
    ]) {
      const query = parseResultsQuery({ page: "2", limit: "13", ...filters });
      assert.deepEqual(
        json(await store.aggregatePage(query)),
        json(paginateResults(snapshot, query)),
        JSON.stringify(filters),
      );
    }
  },
);

test(
  "projection persists across restart and detects peer enrichment, retirement, restoration and duplicate imports",
  enabled,
  async (t) => {
    const f = await fixture(t);
    const { store } = f;
    const query = parseResultsQuery({ page: "1", limit: "50", sort: "ippure" });
    const node = {
      name: "Link",
      type: "vless",
      server: "example.org",
      port: 443,
      country: "US",
      rawLink: "vless://credential@example.org:443#Old",
    };
    const batch = { checkedAt: "2026-10-11T00:00:00Z", nodes: [node] };
    await store.syncSnapshot(batch);
    const first = await store.get();
    await store.refreshAggregate();
    const peer = await f.open();
    await peer.saveIPPure(first.id, 0, {
      status: "success",
      country: "JP",
      score: 0,
      checkedAt: "2026-10-12T00:00:00Z",
    });
    assert.equal((await store.aggregatePage(query)).nodes[0].ippure, undefined);
    await store.refreshAggregate();
    let page = await store.aggregatePage(query);
    assert.equal(page.nodes[0].country, "JP");
    assert.equal(page.nodes[0].ippure.score, 0);
    await peer.syncSnapshot({
      checkedAt: "2026-10-13T00:00:00Z",
      nodes: [
        {
          ...node,
          name: "Updated",
          rawLink: "vless://credential@example.org:443#New",
        },
      ],
    });
    await store.refreshAggregate();
    page = await store.aggregatePage(query);
    assert.equal(page.nodes[0].name, "Updated");
    assert.equal(page.nodes[0].ippure.score, 0);
    assert.deepEqual(
      json(page),
      json(paginateResults(await f.reference.aggregate(), query)),
    );

    const reason =
      "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed";
    await peer.retireNodes(page.nodes, reason);
    const retiredRefresh = await store.refreshAggregate();
    assert.equal(retiredRefresh.incremental, true);
    assert.equal(retiredRefresh.batchesRead, 0);
    assert.equal((await store.aggregatePage(query)).total, 0);
    assert.equal((await store.get(first.id)).nodes.length, 1);
    const retirement = (await peer.retiredNodes())[0];
    await peer.restoreNode(aggregateIdentity(node), retirement.retiredAt);
    await store.refreshAggregate();
    assert.equal((await store.aggregatePage(query)).total, 1);
    await peer.syncSnapshot(batch);
    assert.equal((await store.refreshAggregate()).updated, false);
    const reopened = await f.open();
    assert.deepEqual(
      json(await reopened.aggregatePage(query)),
      json(await store.aggregatePage(query)),
    );

    // Publishing in a peer must invalidate this worker's in-memory source
    // cache even when the database is already clean by the next read.
    const latest = await peer.get();
    await peer.saveIPPure(latest.id, 0, {
      status: "success",
      score: 7,
      country: "SG",
      exitIPMode: "proxy-echo",
      exitIPVersion: 3,
      asnOrganization: "Verified",
      checkedAt: "2026-10-14T00:00:00Z",
    });
    await peer.aggregate();
    assert.equal((await store.aggregate()).nodes[0].ippure.score, 7);
    assert.equal(
      (await store.ippureCandidates()).some(
        (item) => item.snapshotId === latest.id,
      ),
      false,
    );
    assert.deepEqual(
      json(await store.aggregate({ includeRetired: true })),
      json(await f.reference.aggregate({ includeRetired: true })),
    );

    await f.admin.query(
      `UPDATE "${f.schema}".node_explorer_snapshots SET payload = '{broken'`,
    );
    assert.equal((await store.aggregatePage(query)).total, 1);
    await assert.rejects(store.aggregate());
    await assert.rejects(store.refreshAggregate());
    assert.equal((await store.aggregatePage(query)).total, 1);
  },
);

test(
  "empty projections and disabled detection filters preserve API semantics",
  enabled,
  async (t) => {
    const { store, reference } = await fixture(t);
    const query = parseResultsQuery({
      page: "9",
      limit: "50",
      minSpeed: "100",
      media: "openai",
      sort: "speed",
      direction: "desc",
    });

    assert.equal(await store.aggregatePage(query), null);
    await store.syncSnapshot({ nodes: [] });
    await store.refreshAggregate();
    assert.deepEqual(
      json(await store.aggregatePage(query)),
      json(paginateResults(await reference.aggregate(), query)),
    );
    await store.syncSnapshot({
      nodes: [{ name: "节点 10" }, { name: "节点 2" }],
    });
    await store.refreshAggregate();
    const actual = await store.aggregatePage(query);
    assert.deepEqual(
      json(actual),
      json(paginateResults(await reference.aggregate(), query)),
    );
    assert.deepEqual(
      actual.nodes.map((n) => n.name),
      ["节点 2", "节点 10"],
    );
  },
);

test(
  "projection publication is atomic and concurrent imports remain dirty for the next rebuild",
  enabled,
  async (t) => {
    const f = await fixture(t);
    const query = parseResultsQuery({ page: "1" });
    await f.store.syncSnapshot({ nodes: [{ name: "First" }] });
    await f.store.refreshAggregate();
    await f.store.syncSnapshot({ nodes: [{ name: "Second" }] });
    const pool = new pg.Pool(f.config);
    t.after(() => pool.end());
    let insertedDuringBuild = false,
      observedOldGeneration = false;
    const observedPool = {
      async connect() {
        const client = await pool.connect();
        return {
          release: () => client.release(),
          async query(sql, values) {
            const result = await client.query(sql, values);
            if (sql === "COMMIT" && !insertedDuringBuild) {
              insertedDuringBuild = true;
              await f.store.syncSnapshot({ nodes: [{ name: "Concurrent" }] });
            }
            if (sql === "DELETE FROM node_explorer_current_nodes") {
              assert.equal((await f.store.aggregatePage(query)).total, 1);
              observedOldGeneration = true;
            }
            return result;
          },
        };
      },
    };
    await refreshResultsProjection(observedPool);
    assert.equal(observedOldGeneration, true);
    assert.equal((await f.store.aggregatePage(query)).total, 2);
    assert.equal((await f.store.refreshAggregate()).updated, true);
    assert.equal((await f.store.aggregatePage(query)).total, 3);
  },
);

test(
  "incremental refresh reads only changed batches and reconciled history preserves enrichment indexes",
  enabled,
  async (t) => {
    const f = await fixture(t);
    const query = parseResultsQuery({ page: "1", limit: "50" });
    const first = {
      name: "Archive",
      type: "http",
      server: "archive.example",
      port: 443,
    };
    const second = {
      name: "Latest",
      type: "http",
      server: "latest.example",
      port: 443,
    };
    await f.store.syncSnapshot({
      checkedAt: "2026-10-10T00:00:00Z",
      nodes: [first],
    });
    const archived = await f.store.get();
    await f.store.syncSnapshot({
      checkedAt: "2026-10-11T00:00:00Z",
      nodes: [second],
    });
    const latest = await f.store.get();
    await f.store.refreshAggregate();
    await f.store.saveIPPure(latest.id, 0, {
      status: "success",
      score: 42,
    });
    const refreshed = await f.store.refreshAggregate();
    assert.equal(refreshed.incremental, true);
    assert.equal(refreshed.batchesRead, 1);
    assert.deepEqual(
      json(await f.store.aggregatePage(query)),
      json(paginateResults(await f.reference.aggregate(), query)),
    );
    await f.store.reconcileSources(new Map(), { prune: true });
    await f.store.refreshAggregate();
    assert.deepEqual(
      json(await f.store.aggregatePage(query)),
      json(paginateResults(await f.reference.aggregate(), query)),
    );
    assert.equal((await f.store.get(archived.id)).nodes.length, 0);
  },
);

test(
  "pruneOldSnapshots deletes expired batches once and throttles within the hour",
  enabled,
  async (t) => {
    const f = await fixture(t);
    await f.store.syncSnapshot({ nodes: [{ name: "Old" }] });
    await f.store.syncSnapshot({ nodes: [{ name: "Keep" }] });
    const ids = (
      await f.admin.query(
        `SELECT id FROM "${f.schema}".node_explorer_snapshots ORDER BY id`,
      )
    ).rows.map((row) => row.id);
    const pool = new pg.Pool(f.config);
    t.after(() => pool.end());
    const cache = {};
    // Retention disabled.
    assert.deepEqual(await pruneOldSnapshots(pool, cache, 0), { pruned: 0 });
    await f.admin.query(
      `UPDATE "${f.schema}".node_explorer_snapshots SET imported_at = '2020-01-01T00:00:00.000Z' WHERE id = $1`,
      [ids[0]],
    );
    assert.deepEqual(await pruneOldSnapshots(pool, cache, 7), { pruned: 1 });
    assert.equal(await f.store.latestSnapshotId(), ids[1]);
    assert.equal(
      (
        await f.admin.query(
          `SELECT COUNT(*)::int AS count FROM "${f.schema}".node_explorer_ippure_results WHERE snapshot_id = $1`,
          [ids[0]],
        )
      ).rows[0].count,
      0,
    );
    // Throttled: a newly expired batch survives until the next window.
    await f.admin.query(
      `UPDATE "${f.schema}".node_explorer_snapshots SET imported_at = '2020-01-01T00:00:00.000Z' WHERE id = $1`,
      [ids[1]],
    );
    assert.deepEqual(await pruneOldSnapshots(pool, cache, 7), { pruned: 0 });
    assert.equal(
      (
        await f.admin.query(
          `SELECT COUNT(*)::int AS count FROM "${f.schema}".node_explorer_snapshots`,
        )
      ).rows[0].count,
      1,
    );
  },
);

test("departed panel members leave the results view while live and returning nodes stay", () => {
  const base = Date.parse("2026-10-14T22:00:00.000Z");
  const ago = (minutes) => new Date(base - minutes * 60000).toISOString();
  const make = (name) => ({
    name,
    type: "vless",
    server: `${name}.example`,
    port: 443,
    rawConfig: {
      name,
      type: "vless",
      server: `${name}.example`,
      port: 443,
      uuid: "u",
    },
  });
  const goneLong = make("gone-long");
  const returning = make("returning");
  const live = make("live");
  const subsStale = make("subs-stale");
  const subsGone = make("subs-gone");
  const neverSeen = make("never-seen");
  const row = (id, minutes, kind, nodes) => ({
    id,
    importedAt: ago(minutes),
    snapshot: {
      nodes: [{ subTag: kind === "qilan" ? "Qilan" : undefined }, ...nodes],
    },
    nodeIdentities: nodes.map(aggregateIdentity),
  });
  const snapshots = [
    row(1, 180, "qilan", [goneLong, returning]),
    row(2, 300, "subs", [subsGone]),
    row(3, 40, "subs", [subsStale]),
    row(4, 20, "qilan", [returning, live]),
  ];
  const nodes = [goneLong, subsGone, subsStale, returning, live, neverSeen];
  const kept = filterDepartedNodes(nodes, snapshots)
    .map((node) => node.name)
    .sort();
  // Qilan absent for 3 h (> 45 min) and subs absent for 5 h (> 4 h) drop;
  // a subs node missing one round stays; the reference time is the newest
  // import, not the wall clock, so backfilled snapshots behave identically.
  assert.deepEqual(kept, ["live", "never-seen", "returning", "subs-stale"]);
  // Empty snapshot history keeps every row.
  assert.deepEqual(filterDepartedNodes(nodes, []), nodes);
});
