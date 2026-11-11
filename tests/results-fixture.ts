import type { Route } from "@playwright/test";
import {
  paginateResults,
  parseResultsQuery,
} from "./results-query-fixture.js";

export function fulfillRoute(
  route: Route,
  options: Parameters<Route["fulfill"]>[0],
) {
  const url = new URL(route.request().url());
  if (url.pathname === "/api/results" && options.json?.nodes) {
    // The archived pagination fixture predates residential preference. Its
    // fixtures have no runtime evidence, so preferred falls back to speed/name.
    if (url.searchParams.get("sort") === "preferred") {
      url.searchParams.set("sort", options.json.speedTest ? "speed" : "name");
      url.searchParams.set(
        "direction",
        options.json.speedTest ? "desc" : "asc",
      );
    }
    const query = parseResultsQuery(Object.fromEntries(url.searchParams));
    if (!query) throw new Error("Homepage must request a page of results");
    return route.fulfill({
      ...options,
      json: paginateResults(options.json, query),
    });
  }
  return route.fulfill(options);
}
