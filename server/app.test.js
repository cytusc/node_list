import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createApp } from "./app.js";

async function fixture(t) {
  const subDir = await mkdtemp(path.join(os.tmpdir(), "node-explorer-"));
  const resultsFile = path.join(subDir, "results.json");
  const app = createApp({ subDir, resultsFile });
  const server = app.listen(0, "127.0.0.1");
  await new Promise((resolve) => server.once("listening", resolve));
  t.after(async () => {
    await new Promise((resolve) => server.close(resolve));
    await rm(subDir, { recursive: true, force: true });
  });
  return {
    subDir,
    resultsFile,
    get: (url) => fetch(`http://127.0.0.1:${server.address().port}${url}`),
  };
}

test("reads each snapshot afresh and strips credential fields", async (t) => {
  const f = await fixture(t);
  const snapshot = {
    checkedAt: "2026-10-11T08:06:01+08:00",
    speedTest: true,
    mediaCheck: false,
    nodes: [
      {
        name: "HK 1",
        server: "example.org",
        type: "ss",
        speed: 1000,
        password: "secret",
        uuid: "private",
      },
    ],
  };
  await writeFile(f.resultsFile, JSON.stringify(snapshot));
  let response = await f.get("/api/results");
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("cache-control"), "no-store");
  let result = await response.json();
  assert.equal(result.nodes[0].name, "HK 1");
  assert.equal("password" in result.nodes[0], false);
  assert.equal("uuid" in result.nodes[0], false);
  await writeFile(f.resultsFile, JSON.stringify({ ...snapshot, nodes: [] }));
  result = await (await f.get("/api/results")).json();
  assert.deepEqual(result.nodes, []);
});

test("distinguishes missing results from corrupted files without leaking paths", async (t) => {
  const f = await fixture(t);
  let response = await f.get("/api/results");
  assert.equal(response.status, 404);
  assert.equal((await response.json()).code, "NO_RESULTS");
  await writeFile(f.resultsFile, "{broken");
  response = await f.get("/api/results");
  assert.equal(response.status, 503);
  assert.equal((await response.json()).code, "READ_FAILED");
});

test("downloads only allowlisted output files and reports missing formats", async (t) => {
  const f = await fixture(t);
  await mkdir(path.join(f.subDir, "output"));
  await writeFile(path.join(f.subDir, "output", "all.yaml"), "proxies: []\n");
  const list = await (await f.get("/api/downloads")).json();
  assert.equal(list.find((d) => d.id === "clash").available, true);
  assert.equal(list.find((d) => d.id === "mihomo").available, false);
  const response = await f.get("/api/download/clash");
  assert.equal(response.status, 200);
  assert.match(response.headers.get("content-disposition"), /all.yaml/);
  assert.equal(await response.text(), "proxies: []\n");
  assert.equal((await f.get("/api/download/mihomo")).status, 404);
  assert.equal((await f.get("/api/download/constructor")).status, 404);
  assert.equal((await f.get("/api/download/%2e%2e%2fconfig.yaml")).status, 404);
});
