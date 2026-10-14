import { readFile } from "node:fs/promises";
import { parse } from "yaml";
import { createHash } from "node:crypto";

function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.keys(value)
        .sort()
        .map((key) => [key, canonical(value[key])]),
    );
  return value;
}

function connectionConfig(proxy) {
  const { name, ...config } = proxy;
  // Omitted transport/flags and their explicit defaults are equivalent. Keep
  // the original object intact for storage and normalize only for comparison.
  if (config.network === "tcp") delete config.network;
  if (config.tls === false) delete config.tls;
  if (config.udp === false) delete config.udp;
  if (config.type === "vless" && config.encryption === "none")
    delete config.encryption;
  // Mihomo HTTP outbound sends Basic auth only when BOTH fields are nonempty.
  // Upstream sources sometimes put the literal "None" in an unused password.
  if (config.type === "http" && (!config.username || !config.password)) {
    delete config.username;
    delete config.password;
  }
  return canonical(config);
}

export function sourceKey(node) {
  return JSON.stringify([nodeIdentity(node), node.name]);
}

export function aggregateIdentity(node) {
  if (!node.rawConfig && node.rawLink) {
    // Share links contain credentials and transport settings. Display names and
    // Qilan's source IDs, latency, status and ISP describe a check, not a connection.
    try {
      let connection;
      if (node.rawLink.startsWith("vmess://")) {
        const { ps, name, ...config } = JSON.parse(
          Buffer.from(node.rawLink.slice(8), "base64").toString("utf8"),
        );
        connection = JSON.stringify(canonical(config));
      } else {
        const url = new URL(node.rawLink);
        url.hash = "";
        url.searchParams.sort();
        if (url.protocol === "hy2:") url.protocol = "hysteria2:";
        connection = url.toString();
      }
      return "share-link:" + createHash("sha256").update(connection).digest("hex");
    } catch {
      // Incomplete legacy links retain the summary-based identity.
    }
  }
  if (!node.rawConfig) return nodeIdentity(node);
  return (
    nodeIdentity(node) +
    createHash("sha256")
      .update(JSON.stringify(connectionConfig(node.rawConfig)))
      .digest("hex")
  );
}

const detailPaths = {
  cipher: ["cipher"],
  flow: ["flow"],
  "client-fingerprint": ["client-fingerprint"],
  alpn: ["alpn"],
  "skip-cert-verify": ["skip-cert-verify"],
  "ws-path": ["ws-opts", "path"],
  "ws-host": ["ws-opts", "headers", "Host"],
  "grpc-service-name": ["grpc-opts", "grpc-service-name"],
  "h2-path": ["h2-opts", "path"],
  "http-path": ["http-opts", "path"],
  plugin: ["plugin"],
  obfs: ["obfs"],
  protocol: ["protocol"],
  up: ["up"],
  down: ["down"],
  ports: ["ports"],
  "congestion-controller": ["congestion-controller"],
  "udp-relay-mode": ["udp-relay-mode"],
  version: ["version"],
};

export function nodeIdentity(node) {
  const details = (node.details || [])
    .map(({ k, v }) => [k, v])
    .sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
  return JSON.stringify([
    node.type?.toLowerCase(),
    node.server?.toLowerCase(),
    String(node.port ?? ""),
    node.sni?.toLowerCase() || "",
    node.network?.toLowerCase() || "",
    !!node.tls,
    !!node.reality,
    !!node.udp,
    details,
    node.server ? "" : node.baseName || node.name,
  ]);
}

// Mirrors the upstream results summary, so a reused display name never supplies
// another endpoint's credentials. Arrays use the upstream comma-space format.
export function summarizeProxy(proxy) {
  const type = proxy.type;
  const quic = ["hysteria", "hysteria2", "tuic"].includes(type);
  const reality =
    !!proxy["reality-opts"] && Object.keys(proxy["reality-opts"]).length > 0;
  const tls =
    ["trojan", "anytls"].includes(type) || quic || !!proxy.tls || reality;
  const format = (v) =>
    Array.isArray(v)
      ? v.map(format).filter(Boolean).join(", ")
      : v && typeof v === "object"
        ? ""
        : String(v).trim();
  return {
    name: proxy.name,
    type,
    server: proxy.server,
    port: String(proxy.port),
    tls,
    reality,
    sni: tls ? proxy.servername || proxy.sni || "" : "",
    udp: quic || !!proxy.udp,
    network:
      proxy.network || (quic ? "quic" : type === "wireguard" ? "udp" : "tcp"),
    details: Object.entries(detailPaths).flatMap(([k, keys]) => {
      const v = keys.reduce((value, key) => value?.[key], proxy);
      return v === undefined || v === null || format(v) === ""
        ? []
        : [{ k, v: format(v) }];
    }),
  };
}

export function parseShareLinks(raw) {
  const decoded = raw.includes("://")
    ? raw
    : Buffer.from(raw.replace(/\s+/g, ""), "base64").toString("utf8");
  const links = new Map();
  for (const line of decoded.split(/\r?\n/)) {
    const value = line.trim();
    if (!/^[a-z][a-z\d+.-]*:\/\//i.test(value)) continue;
    try {
      const name = decodeURIComponent(new URL(value).hash.slice(1));
      if (!name) continue;
      if (!links.has(name)) links.set(name, new Set());
      links.get(name).add(value);
    } catch {
      /* Invalid links cannot establish provenance. */
    }
  }
  return new Map(
    [...links]
      .filter(([, values]) => values.size === 1)
      .map(([name, values]) => [name, [...values][0]]),
  );
}

export async function readNodeSources(options) {
  // String form supports sources that publish only a share subscription.
  if (typeof options === "string") {
    const links = parseShareLinks(await readFile(options, "utf8"));
    if (!links.size) throw new Error("No usable source links");
    return { links, configs: null };
  }
  const document = parse(await readFile(options.clashFile, "utf8"));
  if (!Array.isArray(document?.proxies))
    throw new Error("Invalid source configuration");
  let links = new Map();
  if (options.linksFile) {
    try {
      links = parseShareLinks(await readFile(options.linksFile, "utf8"));
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
  }
  const configs = new Map();
  for (const proxy of document.proxies) {
    if (!proxy?.type || !proxy.server || !proxy.port)
      throw new Error("Incomplete source configuration");
    const key = nodeIdentity(summarizeProxy(proxy));
    if (!configs.has(key)) configs.set(key, []);
    configs.get(key).push(proxy);
  }
  return { links, configs };
}

export function sourceForNode(node, sources, { exactNames = false } = {}) {
  if (!sources.configs) {
    const rawLink = sources.links.get(node.name);
    return rawLink && linkMatchesConfig(rawLink, node) ? { rawLink } : null;
  }
  let matches = sources.configs.get(nodeIdentity(node)) || [];
  if (exactNames) matches = matches.filter((proxy) => proxy.name === node.name);
  // Credentials were omitted from old summaries. Ambiguous configurations must
  // not be arbitrarily attached to an archived node.
  const variants = new Map();
  for (const proxy of matches) {
    const key = JSON.stringify(connectionConfig(proxy));
    if (!variants.has(key)) variants.set(key, proxy);
  }
  if (variants.size !== 1) return null;
  const rawConfig = [...variants.values()][0];
  const candidate = sources.links.get(rawConfig.name);
  const rawLink =
    candidate && linkMatchesConfig(candidate, rawConfig)
      ? candidate
      : undefined;
  return { rawConfig, ...(rawLink ? { rawLink } : {}) };
}

function linkMatchesConfig(link, config) {
  try {
    const url = new URL(link);
    const type = url.protocol.slice(0, -1);
    if (
      (type === "hy2" ? "hysteria2" : type) !== config.type ||
      url.hostname.replace(/^\[|\]$/g, "").toLowerCase() !==
        config.server.toLowerCase() ||
      String(url.port || (url.protocol === "https:" ? 443 : "")) !==
        String(config.port)
    )
      return false;
    const user = decodeURIComponent(url.username);
    const password = decodeURIComponent(url.password);
    if (config.type === "vless" && config.uuid) return user === config.uuid;
    if (
      ["trojan", "hysteria2", "anytls"].includes(config.type) &&
      config.password
    )
      return (
        (password ? `${user}:${password}` : user) === String(config.password)
      );
    if (config.type === "ss" && config.cipher && config.password) {
      const auth = password
        ? `${user}:${password}`
        : Buffer.from(user, "base64").toString("utf8");
      return auth === `${config.cipher}:${config.password}`;
    }
    return !config.password || password === String(config.password);
  } catch {
    return false;
  }
}

export function reconcileSnapshot(snapshot, linkedNodes, prune) {
  const indexes = new Map();
  let changed = false;
  const nodes = snapshot.nodes.flatMap((node, oldIndex) => {
    const source = linkedNodes.get(sourceKey(node));
    // Captured credentials describe their own detection round. Never overwrite
    // archived source data with a later subscription's changed credentials.
    const updated =
      node.rawConfig || node.rawLink ? node : { ...node, ...source };
    if (prune && !updated.rawLink && !updated.rawConfig) {
      changed = true;
      return [];
    }
    indexes.set(oldIndex, indexes.size);
    if (JSON.stringify(updated) !== JSON.stringify(node)) changed = true;
    return [updated];
  });
  return { snapshot: { ...snapshot, nodes }, indexes, changed };
}
