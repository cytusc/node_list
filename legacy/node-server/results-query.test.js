import { test } from "node:test";
import assert from "node:assert/strict";
import {
  paginateResults,
  parseResultsQuery,
  resultsMetadata,
} from "./results-query.js";

const snapshot = {
  speedTest: false,
  mediaCheck: false,
  nodes: [
    {
      name: "A",
      ippure: {
        status: "success",
        source: "原生IP",
        attributes: ["住宅IP", "IPv4", "IPv4"],
      },
    },
    {
      name: "B",
      ippure: {
        status: "success",
        source: "广播IP",
        attributes: ["机房IP", "IPv4"],
      },
    },
    {
      name: "C",
      ippure: { status: "success", source: "广播IP", attributes: ["住宅IP"] },
    },
    {
      name: "Failed",
      ippure: { status: "error", source: "原生IP", attributes: ["住宅IP"] },
    },
    { name: "Pending" },
  ],
};

test("IP facets count successful detections and each attribute once per node", () => {
  const { facets } = resultsMetadata(snapshot);
  assert.deepEqual(facets.ipSource, [
    { value: "广播IP", label: "广播IP", count: 2 },
    { value: "原生IP", label: "原生IP", count: 1 },
  ]);
  assert.deepEqual(facets.ipAttributes, [
    { value: "住宅IP", label: "住宅IP", count: 2 },
    { value: "IPv4", label: "IPv4", count: 2 },
    { value: "机房IP", label: "机房IP", count: 1 },
  ]);
});

test("IP filters combine alternatives within each group and intersect groups before pagination", () => {
  const page = (filters) =>
    paginateResults(
      snapshot,
      parseResultsQuery({
        page: "1",
        sort: "name",
        direction: "asc",
        ...filters,
      }),
    );
  assert.deepEqual(
    page({ ipSource: "原生IP,广播IP" }).nodes.map((n) => n.name),
    ["A", "B", "C"],
  );
  assert.deepEqual(
    page({ ipAttributes: "住宅IP,机房IP" }).nodes.map((n) => n.name),
    ["A", "B", "C"],
  );
  const filtered = page({
    ipSource: "广播IP",
    ipAttributes: "住宅IP,missing",
    limit: "1",
  });
  assert.equal(filtered.total, 1);
  assert.equal(filtered.nodes[0].name, "C");
  assert.deepEqual(filtered.facets, resultsMetadata(snapshot).facets);
  assert.equal(page({ ipAttributes: "missing" }).total, 0);
  assert.equal(page({}).total, 5);
});

test("IP filter query parameters reject repeated or non-string values", () => {
  for (const key of ["ipSource", "ipAttributes"])
    assert.throws(
      () => parseResultsQuery({ page: "1", [key]: ["a", "b"] }),
      new RegExp(key),
    );
});

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
