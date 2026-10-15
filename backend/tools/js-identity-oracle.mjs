// Cross-check the Go identity against the real JavaScript implementation by
// importing the archived repository module directly. Run from D:/code/node_list.
// Usage: node backend/tools/js-identity-oracle.mjs <nodes.jsonl>
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { aggregateIdentity, nodeIdentity } from "../../legacy/node-server/node-sources.js";

const path = process.argv[2];
const data = readFileSync(path, "utf8");
const lines = data.split("\n");
const chunks = [];
let total = 0;
let byBranch = { share: 0, summary: 0, clash: 0 };
for (const line of lines) {
  if (!line.trim()) continue;
  total++;
  const node = JSON.parse(line);
  const identity = aggregateIdentity(node);
  let branch = "clash";
  if (!node.rawConfig && node.rawLink) {
    try {
      new URL(node.rawLink);
      branch = "share";
    } catch {
      branch = "summary";
    }
  } else if (!node.rawConfig) {
    branch = "summary";
  }
  byBranch[branch]++;
  chunks.push(
    String(node.rawLink ?? "").replace(/\t/g, " ") +
      "\t" +
      identity +
      "\t" +
      branch +
      "\n",
  );
}
process.stdout.write(chunks.join(""));
console.error("jsscan2: total=" + total + " " + JSON.stringify(byBranch));
