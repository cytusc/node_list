import path from "node:path";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createPostgresStore } from "./postgres-store.js";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const input = process.argv[2];
if (!input) throw new Error("Usage: node server/retire-nodes.js targets.json");
const targets = JSON.parse(await readFile(input, "utf8"));
if (!Array.isArray(targets) || targets.some((item) =>
  !Number.isSafeInteger(item.snapshotId) ||
  typeof item.name !== "string" ||
  typeof item.server !== "string"
)) throw new Error("Invalid retirement target list");

const store = await createPostgresStore();
try {
  const nodes = [];
  for (const target of targets) {
    const snapshot = await store.get(target.snapshotId);
    const matches = (snapshot?.nodes || []).filter(
      (node) => node.name === target.name && node.server === target.server,
    );
    if (matches.length !== 1 || !matches[0].rawConfig)
      throw new Error(`Target is missing or ambiguous in snapshot ${target.snapshotId}`);
    nodes.push(matches[0]);
  }
  const count = await store.retireNodes(nodes, "GitHub probe failed twice");
  console.log(JSON.stringify({ retired: count }));
} finally {
  await store.close();
}
