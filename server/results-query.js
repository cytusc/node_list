const regions = new Intl.DisplayNames(["zh-CN"], { type: "region" });
export function countryName(code) {
  if (!code || code === "unknown") return "未知地区";
  try {
    return regions.of(code.toUpperCase()) || code;
  } catch {
    return code;
  }
}
const platform = (m) => m.platform || m.Platform || "";
const mediaValue = (m) => m.tag || m.Tag || "";
export const resultSortKeys = [
  "country",
  "name",
  "type",
  "speed",
  "asnOrganization",
  "ipSource",
  "ipAttributes",
  "ippure",
  "trafficRatio",
];

export function parseResultsQuery(params) {
  if (params.page === undefined && params.limit === undefined) return null;
  if (
    (params.page !== undefined && typeof params.page !== "string") ||
    (params.limit !== undefined && typeof params.limit !== "string")
  )
    throw new Error("pagination");
  const page = Number(params.page ?? 1);
  const limit = Number(params.limit ?? 50);
  if (
    !Number.isSafeInteger(page) ||
    page < 1 ||
    !Number.isSafeInteger(limit) ||
    limit < 1 ||
    limit > 200
  )
    throw new Error("pagination");
  for (const key of [
    "q",
    "country",
    "type",
    "network",
    "features",
    "ipSource",
    "ipAttributes",
    "media",
    "minSpeed",
    "sort",
    "direction",
  ]) {
    if (params[key] !== undefined && typeof params[key] !== "string")
      throw new Error(key);
  }
  if (params.sort && !resultSortKeys.includes(params.sort))
    throw new Error("sort");
  if (params.direction && !["asc", "desc"].includes(params.direction))
    throw new Error("direction");
  const minSpeed = Number(params.minSpeed || 0);
  if (!Number.isFinite(minSpeed) || minSpeed < 0) throw new Error("speed");
  const list = (key) => (params[key] || "").split(",").filter(Boolean);
  const features = list("features");
  if (features.some((key) => !["tls", "reality", "udp"].includes(key)))
    throw new Error("features");
  return {
    page,
    limit,
    q: (params.q || "").trim().toLocaleLowerCase(),
    countries: list("country"),
    types: list("type"),
    networks: list("network"),
    features,
    ipSources: list("ipSource"),
    ipAttributes: list("ipAttributes"),
    media: list("media"),
    minSpeed,
    sort: params.sort,
    direction: params.direction || "desc",
  };
}

export function resultsMetadata(snapshot) {
  const nodes = snapshot.nodes;
  const counts = (key) => {
    const values = new Map();
    for (const node of nodes) {
      const value = node[key] || "unknown";
      values.set(value, (values.get(value) || 0) + 1);
    }
    return [...values]
      .sort((a, b) => b[1] - a[1])
      .map(([value, count]) => ({
        value,
        count,
        label: key === "country" ? countryName(value) : value.toUpperCase(),
      }));
  };
  const ipCounts = (key) => {
    const values = new Map();
    for (const node of nodes) {
      if (node.ippure?.status !== "success") continue;
      const items =
        key === "source" ? [node.ippure.source] : node.ippure.attributes || [];
      for (const value of new Set(items.filter(Boolean))) {
        values.set(value, (values.get(value) || 0) + 1);
      }
    }
    return [...values]
      .sort((a, b) => b[1] - a[1])
      .map(([value, count]) => ({ value, label: value, count }));
  };
  const speeds = nodes
    .map((n) => n.speed || 0)
    .filter((s) => s > 0)
    .sort((a, b) => a - b);
  const middle = Math.floor(speeds.length / 2);
  const summary = {
    countries: new Set(nodes.map((n) => n.country).filter(Boolean)).size,
    maxSpeed: speeds.at(-1) || 0,
    median: speeds.length
      ? speeds.length % 2
        ? speeds[middle]
        : (speeds[middle - 1] + speeds[middle]) / 2
      : 0,
  };
  const facets = {
    country: counts("country"),
    type: counts("type"),
    network: counts("network"),
    ipSource: ipCounts("source"),
    ipAttributes: ipCounts("attributes"),
    features: ["tls", "reality", "udp"].map((value) => ({
      value,
      label: value === "reality" ? "Reality" : value.toUpperCase(),
      count: nodes.filter((n) => n[value]).length,
    })),
    media: [...new Set(nodes.flatMap((n) => (n.media || []).map(platform)))]
      .filter(Boolean)
      .map((value) => ({
        value,
        label: value,
        count: nodes.filter((n) =>
          n.media?.some((m) => platform(m) === value && mediaValue(m)),
        ).length,
      })),
  };
  const { nodes: _nodes, sourceWarning: _warning, ...metadata } = snapshot;
  return { ...metadata, totalNodes: nodes.length, summary, facets };
}

export function resultSearchValues(n) {
  return [
    n.name,
    n.baseName,
    n.server,
    n.sni,
    n.country,
    countryName(n.country),
  ]
    .filter((v) => v != null)
    .map((v) => v.toLocaleLowerCase());
}

export function resultsSort(snapshot, query) {
  return {
    key:
      query.sort === "speed" && !snapshot.speedTest
        ? "name"
        : query.sort || (snapshot.speedTest ? "speed" : "name"),
    direction:
      query.sort === "speed" && !snapshot.speedTest ? "asc" : query.direction,
  };
}

export function resultSortValue(n, sort) {
  if (sort === "name")
    return (
      (n.baseName || n.name || "")
        .replace(/[\u{1F1E6}-\u{1F1FF}]/gu, "")
        .trim() || n.name
    );
  if (["country", "type", "speed"].includes(sort)) return n[sort];
  if (n.ippure?.status !== "success") return undefined;
  return {
    asnOrganization: n.ippure.asnOrganization,
    ipSource: n.ippure.source,
    ipAttributes: n.ippure.attributes?.join(" · "),
    ippure: n.ippure.score,
    trafficRatio: n.ippure.humanPercent,
  }[sort];
}

export function compareResultValues(left, right) {
  return typeof left === "number" && typeof right === "number"
    ? left - right
    : String(left).localeCompare(String(right), "zh-CN", { numeric: true });
}

export function paginateResults(snapshot, query) {
  const nodes = snapshot.nodes;
  const matches = nodes.filter(
    (n) =>
      (!query.q || resultSearchValues(n).some((v) => v.includes(query.q))) &&
      (!query.countries.length ||
        query.countries.includes(n.country || "unknown")) &&
      (!query.types.length || query.types.includes(n.type || "unknown")) &&
      (!query.networks.length ||
        query.networks.includes(n.network || "unknown")) &&
      query.features.every((key) => !!n[key]) &&
      (!query.ipSources?.length ||
        (n.ippure?.status === "success" &&
          query.ipSources.includes(n.ippure.source))) &&
      (!query.ipAttributes?.length ||
        (n.ippure?.status === "success" &&
          n.ippure.attributes?.some((value) =>
            query.ipAttributes.includes(value),
          ))) &&
      (!snapshot.speedTest || (n.speed || 0) >= query.minSpeed * 1024) &&
      (!snapshot.mediaCheck ||
        query.media.every((p) =>
          n.media?.some((m) => platform(m) === p && mediaValue(m)),
        )),
  );
  const { key: sort, direction: order } = resultsSort(snapshot, query);
  const direction = order === "desc" ? -1 : 1;
  matches.sort((a, b) => {
    const left = resultSortValue(a, sort),
      right = resultSortValue(b, sort);
    if (left == null) return right == null ? 0 : 1;
    if (right == null) return -1;
    return direction * compareResultValues(left, right);
  });
  const page = Math.min(
    query.page,
    Math.max(1, Math.ceil(matches.length / query.limit)),
  );
  return {
    ...snapshot,
    ...resultsMetadata(snapshot),
    nodes: matches.slice((page - 1) * query.limit, page * query.limit),
    total: matches.length,
    page,
    limit: query.limit,
  };
}
