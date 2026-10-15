// Explicit maintenance command. Dry-run by default; normal server imports
// never prune history. Run only after taking a database backup and stopping
// writers when using --apply --prune (the collector saves positional indexes).
import { createStore } from "./store.js";
import { createPostgresStore } from "./postgres-store.js";
import {
  readNodeSources,
  sourceForNode,
  sourceKey,
  nodeIdentity,
  aggregateIdentity,
  reconcileSnapshot,
} from "./node-sources.js";

const args = process.argv.slice(2);
const clashFiles = [];
const datedArchives = [];
let linksFile;
let apply = false;
let prune = false;
for (let index = 0; index < args.length; index++) {
  const arg = args[index];
  if (arg === "--clash" || arg === "--links") {
    const file = args[++index];
    if (!file || file.startsWith("--"))
      throw new Error("Missing source file argument");
    if (arg === "--clash") clashFiles.push(file);
    else linksFile = file;
  } else if (arg === "--apply") apply = true;
  else if (arg === "--prune") prune = true;
  else if (arg === "--archive") {
    const value = args[++index] || "";
    const separator = value.indexOf("=");
    const timestamp = Date.parse(value.slice(0, separator));
    if (separator < 1 || !Number.isFinite(timestamp))
      throw new Error("Use --archive publication-timestamp=file.yaml");
    datedArchives.push({ timestamp, clashFile: value.slice(separator + 1) });
  } else throw new Error("Unknown maintenance argument");
}
if (!clashFiles.length)
  throw new Error(
    "Supply --clash with the current all.yaml; additional archived YAML sources may follow",
  );
const current = await readNodeSources({ clashFile: clashFiles[0], linksFile });
if (!current.configs.size)
  throw new Error("Refusing maintenance with an empty source configuration");
const sources = {
  links: current.links,
  configs: new Map(
    [...current.configs].map(([key, values]) => [key, [...values]]),
  ),
};
for (const clashFile of clashFiles.slice(1)) {
  const archived = await readNodeSources({ clashFile });
  for (const [key, configs] of archived.configs) {
    if (!sources.configs.has(key)) sources.configs.set(key, []);
    sources.configs.get(key).push(...configs);
  }
}
for (const archive of datedArchives) {
  archive.sources = await readNodeSources({ clashFile: archive.clashFile });
  for (const [key, configs] of archive.sources.configs) {
    if (!sources.configs.has(key)) sources.configs.set(key, []);
    sources.configs.get(key).push(...configs);
  }
}
datedArchives.sort((a, b) => b.timestamp - a.timestamp);
const store = process.env.PGHOST
  ? await createPostgresStore()
  : createStore(process.env.DATABASE_PATH);
try {
  const rows = [];
  for (let offset = 0; ; offset += 200) {
    const page = await store.history(200, offset);
    for (const item of page.items) rows.push(await store.get(item.id));
    if (offset + page.items.length >= page.total) break;
  }
  const linked = new Map();
  const latestId = rows[0]?.id;
  for (const [index, row] of rows.entries()) {
    // The source was fetched at the start of a detection round, before its
    // results timestamp. The previous completed round bounds that fetch time.
    const start = Date.parse(rows[index + 1]?.checkedAt || row.checkedAt);
    const archive = datedArchives.find((item) => item.timestamp <= start);
    for (const node of row.nodes) {
      const source =
        (row.id === latestId
          ? sourceForNode(node, current, { exactNames: true })
          : null) ||
        sourceForNode(node, sources) ||
        (archive ? sourceForNode(node, archive.sources) : null);
      if (source) linked.set(sourceKey(node), source);
    }
  }
  const before = new Set();
  const after = new Set();
  let removedRecords = 0;
  for (const row of rows) {
    const next = reconcileSnapshot(row, linked, prune).snapshot;
    row.nodes.forEach((node) => before.add(nodeIdentity(node)));
    next.nodes.forEach((node) => after.add(aggregateIdentity(node)));
    const removed = row.nodes.length - next.nodes.length;
    removedRecords += removed;
    console.log(
      JSON.stringify({
        snapshot: row.id,
        before: row.nodes.length,
        after: next.nodes.length,
        removed,
        recoverable: next.nodes.filter((node) => node.rawLink || node.rawConfig)
          .length,
      }),
    );
  }
  console.log(
    JSON.stringify({
      mode: apply ? "apply" : "dry-run",
      prune,
      batches: rows.length,
      beforeUnique: before.size,
      afterUnique: after.size,
      removedRecords,
    }),
  );
  if (apply) {
    await store.reconcileSources(linked, { prune });
    console.log(
      JSON.stringify({
        applied: true,
        batches: (await store.history(1, 0)).total,
        nodes: (await store.aggregate())?.nodes.length || 0,
      }),
    );
  }
} finally {
  await store.close();
}
