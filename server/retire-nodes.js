import path from "node:path";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createPostgresStore } from "./postgres-store.js";
import { aggregateIdentity } from "./node-sources.js";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const input = process.argv[2];
if (!input) throw new Error("Usage: node server/retire-nodes.js targets.json");
const request = JSON.parse(await readFile(input, "utf8"));
const validTargets = (targets) =>
  Array.isArray(targets) &&
  targets.every(
    (item) =>
      Number.isSafeInteger(item.snapshotId) &&
      typeof item.name === "string" &&
      typeof item.server === "string",
  );
if (!validTargets(request.retire) || !validTargets(request.keep))
  throw new Error("Invalid retirement target list");

const store = await createPostgresStore();
try {
  const nodes = [];
  const readTarget = async (target) => {
    const snapshot = await store.get(target.snapshotId);
    const matches = (snapshot?.nodes || []).filter(
      (node) => node.name === target.name && node.server === target.server,
    );
    if (matches.length !== 1 || !matches[0].rawConfig)
      throw new Error(
        `Target is missing or ambiguous in snapshot ${target.snapshotId}`,
      );
    return matches[0];
  };
  for (const target of request.retire) nodes.push(await readTarget(target));
  const keepIdentities = new Set(
    (await Promise.all(request.keep.map(readTarget))).map(aggregateIdentity),
  );
  if (nodes.some((node) => keepIdentities.has(aggregateIdentity(node))))
    throw new Error(
      "Retirement target shares a connection with a reachable node",
    );
  const count = await store.retireNodes(nodes, "GitHub probe failed twice");
  console.log(JSON.stringify({ retired: count, kept: keepIdentities.size }));
} finally {
  await store.close();
}
