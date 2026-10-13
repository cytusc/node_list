import type { Route } from "@playwright/test";
import { paginateResults, parseResultsQuery } from "../server/results-query.js";

export function fulfillRoute(
  route: Route,
  options: Parameters<Route["fulfill"]>[0],
) {
  const url = new URL(route.request().url());
  if (url.pathname === "/api/results" && options.json?.nodes) {
    const query = parseResultsQuery(Object.fromEntries(url.searchParams));
    if (!query) throw new Error("Homepage must request a page of results");
    return route.fulfill({
      ...options,
      json: paginateResults(options.json, query),
    });
  }
  return route.fulfill(options);
}
