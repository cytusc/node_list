import { test } from "node:test";
import assert from "node:assert/strict";
import { patrolOnce, recoverRetiredNodes } from "./github-patrol.js";
import { aggregateIdentity } from "./node-sources.js";

const id = "12345678-1234-1234-1234-123456789abc";
const node = (name) => ({
  name,
  type: "vless",
  server: `${name}.example`,
  port: "443",
  rawLink: `vless://${id}@${name}.example:443?security=tls&type=ws&host=ws.example&path=%2Fws`,
});
function fixture(nodes, overrides = {}) {
  const retired = [];
  return {
    retired,
    options: {
      store: {
        aggregate: async () => ({ nodes: [...nodes] }),
        retiredNodeIdentities: async () => new Set(),
        retireNodes: async (items) => {
          retired.push(...items);
          return items.length;
        },
      },
      controller: "http://controller:9090",
      proxyUrl: "http://proxy:7890",
      signal: new AbortController().signal,
      concurrency: 3,
      retryDelayMs: 1,
      configure: async ({ proxies }) =>
        new Map(proxies.map((p) => [p, `http://${p.name}:7891`])),
      ...overrides,
    },
  };
}

test("share-only nodes are checked twice and retired using their original identity", async () => {
  const good = node("good"),
    bad = node("bad");
  good.ippure = { status: "success" };
  const original = aggregateIdentity(bad),
    calls = [];
  const f = fixture([bad, good], {
    configure: async ({ proxies }) => {
      for (const p of proxies) {
        assert.equal(p.uuid, id);
        assert.deepEqual(p["ws-opts"], {
          path: "/ws",
          headers: { Host: "ws.example" },
        });
      }
      return new Map(proxies.map((p) => [p, `http://${p.name}:7891`]));
    },
    probe: async ({ proxyUrl }) => {
      calls.push(proxyUrl);
      return !proxyUrl?.includes("bad");
    },
  });
  assert.deepEqual(await patrolOnce(f.options), {
    total: 2,
    checked: 2,
    reachable: 1,
    retired: 1,
    deferred: 0,
    skipped: 0,
  });
  assert.deepEqual(f.retired, [bad]);
  assert.equal(bad.rawConfig, undefined);
  assert.equal(aggregateIdentity(f.retired[0]), original);
  assert.equal(calls.filter((url) => url?.includes("bad")).length, 2);
  assert.ok(calls.includes(undefined));
  assert.equal(calls.filter((url) => url?.includes("good")).length, 2);
});

test("malformed links and missing configurations are skipped while rawConfig remains supported", async () => {
  const good = node("good");
  good.rawConfig = {
    name: "good",
    type: "http",
    server: "good.example",
    port: 443,
  };
  good.rawLink = "invalid";
  const f = fixture(
    [
      good,
      { ...node("invalid"), rawLink: "vless://invalid" },
      { name: "missing" },
    ],
    {
      probe: async () => true,
    },
  );
  const result = await patrolOnce(f.options);
  assert.equal(result.checked, 1);
  assert.equal(result.skipped, 2);
  assert.equal(result.retired, 0);
});

test("Mihomo rejects one configuration without blocking supported links in its batch", async () => {
  const f = fixture([node("rejected"), node("good")], {
    configure: async ({ proxies }) => {
      if (proxies.some((p) => p.name === "rejected"))
        throw new Error("代理配置失败 (HTTP 400)");
      return new Map(proxies.map((p) => [p, `http://${p.name}:7891`]));
    },
    probe: async () => true,
  });
  const result = await patrolOnce(f.options);
  assert.equal(result.checked, 1);
  assert.equal(result.reachable, 1);
  assert.equal(result.skipped, 1);
  assert.equal(result.retired, 0);
});

test("failed share-only nodes are deferred when the direct control fails", async () => {
  const f = fixture([node("bad"), node("good")], {
    probe: async ({ proxyUrl }) => !!proxyUrl?.includes("good"),
  });
  const result = await patrolOnce(f.options);
  assert.equal(result.deferred, 1);
  assert.equal(result.retired, 0);
  assert.deepEqual(f.retired, []);
});

test("controller outages abort the patrol without retiring nodes", async () => {
  const f = fixture([node("bad")], {
    configure: async () => {
      throw new Error("代理配置失败 (HTTP 503)");
    },
  });
  await assert.rejects(patrolOnce(f.options), /HTTP 503/);
  assert.deepEqual(f.retired, []);
});

function recoveryFixture() {
  const item = node("recoverable");
  const identity = aggregateIdentity(item);
  const retiredAt = "2026-10-12T00:00:00.000Z";
  const automatic = {
    identity,
    retiredAt,
    reason:
      "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed",
  };
  const manual = { identity: "manual", retiredAt, reason: "Manually disabled" };
  const state = {};
  const restored = [];
  const f = fixture([]);
  const options = {
    ...f.options,
    state,
    now: Date.parse(retiredAt) + 7200000,
    probe: async () => true,
    store: {
      retiredNodes: async () => [automatic, manual],
      aggregate: async (options) => {
        assert.equal(options.includeRetired, true);
        return { nodes: [item] };
      },
      restoreNode: async (...args) => {
        restored.push(args);
        return 1;
      },
    },
  };
  return { options, state, restored, identity, retiredAt };
}

test("recovery requires two successful rounds two hours apart and persists across restarts", async () => {
  const f = recoveryFixture();
  assert.equal(
    (await recoverRetiredNodes({ ...f.options, now: f.options.now - 1 }))
      .checked,
    0,
  );
  assert.equal((await recoverRetiredNodes(f.options)).restored, 0);
  assert.equal(f.state[f.identity].successes, 1);
  assert.equal(
    (await recoverRetiredNodes({ ...f.options, now: f.options.now + 7199999 }))
      .checked,
    0,
  );
  // A new process receives the state loaded from the status JSON file.
  const resumed = JSON.parse(JSON.stringify(f.state));
  const result = await recoverRetiredNodes({
    ...f.options,
    state: resumed,
    now: f.options.now + 7200000,
  });
  assert.equal(result.restored, 1);
  assert.deepEqual(f.restored, [
    [
      f.identity,
      f.retiredAt,
      "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed",
    ],
  ]);
  assert.equal(resumed[f.identity], undefined);
});

test("failed recovery resets consecutive successes and a new retirement starts over", async () => {
  const f = recoveryFixture();
  await recoverRetiredNodes(f.options);
  await recoverRetiredNodes({
    ...f.options,
    now: f.options.now + 7200000,
    probe: async () => false,
  });
  assert.equal(f.state[f.identity].successes, 0);
  await recoverRetiredNodes({ ...f.options, now: f.options.now + 14400000 });
  assert.equal(f.state[f.identity].successes, 1);
  f.state[f.identity].retiredAt = "2026-10-11T00:00:00.000Z";
  f.state[f.identity].successes = 10;
  await recoverRetiredNodes({ ...f.options, now: f.options.now + 21600000 });
  assert.equal(f.state[f.identity].successes, 1);
  assert.deepEqual(f.restored, []);
});

test("controller failures preserve recovery state and never restore a node", async () => {
  const f = recoveryFixture();
  await recoverRetiredNodes(f.options);
  const saved = JSON.stringify(f.state);
  await assert.rejects(
    recoverRetiredNodes({
      ...f.options,
      now: f.options.now + 7200000,
      configure: async () => {
        throw new Error("代理配置失败 (HTTP 503)");
      },
    }),
    /HTTP 503/,
  );
  assert.equal(JSON.stringify(f.state), saved);
  assert.deepEqual(f.restored, []);
});

test("manual retirements are never loaded or probed for automatic recovery", async () => {
  const f = recoveryFixture();
  const result = await recoverRetiredNodes({
    ...f.options,
    store: {
      ...f.options.store,
      retiredNodes: async () => [
        {
          identity: f.identity,
          retiredAt: f.retiredAt,
          reason: "Manually disabled",
        },
      ],
      aggregate: async () => {
        throw new Error("Manual nodes must not be loaded");
      },
    },
    probe: async () => {
      throw new Error("Manual nodes must not be probed");
    },
  });
  assert.equal(result.checked, 0);
  assert.deepEqual(f.restored, []);
});
