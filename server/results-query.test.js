import { test } from "node:test";
import assert from "node:assert/strict";
import { paginateResults, parseResultsQuery } from "./results-query.js";

test("IPPure sorting keeps missing results last in both directions across pages", () => {
  const snapshot = {
    speedTest: false,
    mediaCheck: false,
    nodes: [
      { name: "Pending" },
      { name: "High", ippure: { status: "success", score: 80 } },
      { name: "Low", ippure: { status: "success", score: 10 } },
      { name: "Failed", ippure: { status: "error" } },
    ],
  };
  for (const direction of ["asc", "desc"]) {
    const page = (number) =>
      paginateResults(
        snapshot,
        parseResultsQuery({
          page: String(number),
          limit: "2",
          sort: "ippure",
          direction,
        }),
      );
    assert.deepEqual(
      page(1).nodes.map((n) => n.name),
      direction === "asc" ? ["Low", "High"] : ["High", "Low"],
    );
    assert.deepEqual(
      page(2).nodes.map((n) => n.name),
      ["Pending", "Failed"],
    );
  }
});
