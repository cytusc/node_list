// Convert archived subscriptions locally. Credentials never leave the detector
// and the original link remains the stable connection identity in the store.
export const SHARE_LINK_VERSION = 1;

export class ShareLinkError extends Error {
  constructor() {
    super("节点分享链接格式或连接参数不受当前检测服务支持");
  }
}

const decode64 = (value) => Buffer.from(value, "base64url").toString("utf8");
const flag = (value) => [true, 1, "1", "true"].includes(value);
const list = (value) =>
  String(value || "")
    .split(",")
    .filter(Boolean);
const uuid = (value) => {
  if (
    !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
      value || "",
    )
  )
    throw new ShareLinkError();
  return value;
};

function transport(proxy, get) {
  let network = get("type") || "tcp";
  if (network === "raw") network = "tcp";
  if (
    !["tcp", "ws", "grpc", "http", "h2", "httpupgrade", "xhttp"].includes(
      network,
    )
  )
    throw new ShareLinkError();
  const host = get("host") || get("authority"),
    path = get("path") || "/";
  if (network === "tcp" && get("headerType") === "http") network = "http";
  if (network === "httpupgrade" || network === "ws") {
    proxy.network = "ws";
    proxy["ws-opts"] = {
      path,
      ...(host ? { headers: { Host: host } } : {}),
      ...(network === "httpupgrade" ? { "v2ray-http-upgrade": true } : {}),
      ...(get("ed") || get("max_early_data")
        ? { "max-early-data": Number(get("ed") || get("max_early_data")) }
        : {}),
      ...(get("eh") || get("early_data_header_name")
        ? {
            "early-data-header-name":
              get("eh") || get("early_data_header_name"),
          }
        : {}),
    };
  } else if (network === "grpc") {
    proxy.network = network;
    proxy["grpc-opts"] = {
      "grpc-service-name": get("serviceName") || get("path") || "",
    };
  } else if (network === "http") {
    proxy.network = network;
    proxy["http-opts"] = {
      path: [path],
      ...(host ? { headers: { Host: list(host) } } : {}),
    };
  } else if (network === "h2") {
    proxy.network = network;
    proxy["h2-opts"] = { path, ...(host ? { host: list(host) } : {}) };
  } else if (network === "xhttp") {
    if (proxy.type !== "vless") throw new ShareLinkError();
    proxy.network = network;
    proxy["xhttp-opts"] = {
      path,
      ...(host ? { host } : {}),
      ...(get("mode") ? { mode: get("mode") } : {}),
    };
    if (get("extra")) {
      const extra = JSON.parse(get("extra"));
      const aliases = {
        noGRPCHeader: "no-grpc-header",
        xPaddingBytes: "x-padding-bytes",
        scMaxEachPostBytes: "sc-max-each-post-bytes",
        scMinPostsIntervalMs: "sc-min-posts-interval-ms",
      };
      for (const [key, value] of Object.entries(extra)) {
        if (!["headers", ...Object.keys(aliases)].includes(key))
          throw new ShareLinkError();
        proxy["xhttp-opts"][aliases[key] || key] = value;
      }
    }
  }
}

function tlsSettings(proxy, get, enabled) {
  if (enabled) proxy.tls = true;
  const sni = get("sni") || get("serverName");
  if (sni)
    proxy[
      proxy.type === "vmess" || proxy.type === "vless" ? "servername" : "sni"
    ] = sni;
  if (get("fp")) proxy["client-fingerprint"] = get("fp");
  if (get("alpn")) proxy.alpn = list(get("alpn"));
  if (flag(get("allowInsecure") || get("allowinsecure") || get("insecure")))
    proxy["skip-cert-verify"] = true;
  if (get("pinSHA256")) proxy.fingerprint = get("pinSHA256");
  if (get("ech")) throw new ShareLinkError();
  if (get("security") === "reality") {
    if (!get("pbk")) throw new ShareLinkError();
    proxy["reality-opts"] = {
      "public-key": get("pbk"),
      "short-id": get("sid") || "",
    };
  }
}

function validate(proxy) {
  if (
    !proxy.server ||
    !Number.isInteger(proxy.port) ||
    proxy.port < 1 ||
    proxy.port > 65535
  )
    throw new ShareLinkError();
  const check = (value) => {
    if (typeof value === "string" && /[\x00-\x1f\x7f-\x9f]/.test(value))
      throw new ShareLinkError();
    if (typeof value === "number" && !Number.isFinite(value))
      throw new ShareLinkError();
    if (value && typeof value === "object") Object.values(value).forEach(check);
  };
  check(proxy);
  return proxy;
}

export function shareLinkProxy(link, name = "Archived node") {
  try {
    if (typeof link !== "string") throw new ShareLinkError();
    if (link.startsWith("vmess://")) {
      const config = JSON.parse(decode64(link.slice(8)));
      const proxy = {
        name,
        type: "vmess",
        server: config.add,
        port: Number(config.port),
        uuid: uuid(config.id),
        alterId: Number(config.aid || 0),
        cipher: config.scy || "auto",
        udp: true,
      };
      const get = (key) =>
        ({
          type: config.net,
          headerType: config.type,
          sni: config.sni || config.host,
          path: config.path,
          host: config.host,
          serviceName: config.serviceName,
          fp: config.fp,
          alpn: config.alpn,
          allowInsecure:
            config.allowInsecure ||
            config.insecure ||
            config["skip-cert-verify"],
        })[key];
      tlsSettings(proxy, get, config.tls === "tls" || config.tls === true);
      transport(proxy, get);
      return validate(proxy);
    }
    if (link.startsWith("ssr://")) {
      const [endpoint, query = ""] = decode64(link.slice(6)).split("/?");
      const fields = endpoint.split(":"),
        params = new URLSearchParams(query);
      const [port, protocol, cipher, obfs, password] = fields.splice(-5);
      const proxy = {
        name,
        type: "ssr",
        server: fields.join(":"),
        port: Number(port),
        protocol,
        cipher,
        obfs,
        password: decode64(password),
        udp: true,
      };
      for (const [input, output] of [
        ["obfsparam", "obfs-param"],
        ["protoparam", "protocol-param"],
      ])
        if (params.get(input)) proxy[output] = decode64(params.get(input));
      return validate(proxy);
    }
    // SIP002 permits either base64 userinfo or a base64 full endpoint.
    if (
      link.startsWith("ss://") &&
      !link.slice(5).split(/[?#]/)[0].includes("@")
    ) {
      const tail = link.slice(5),
        end = tail.search(/[?#]/);
      link =
        "ss://" +
        decode64(end < 0 ? tail : tail.slice(0, end)) +
        (end < 0 ? "" : tail.slice(end));
    }
    const url = new URL(link.replaceAll("&amp;", "&"));
    const type =
      { hy2: "hysteria2", socks: "socks5" }[url.protocol.slice(0, -1)] ||
      url.protocol.slice(0, -1);
    const get = (key) => url.searchParams.get(key);
    const user = decodeURIComponent(url.username),
      password = decodeURIComponent(url.password);
    const proxy = {
      name,
      type,
      server: url.hostname.replace(/^\[|\]$/g, ""),
      port: Number(url.port),
      udp: true,
    };
    if (type === "vless") {
      proxy.uuid = uuid(user);
      if (get("flow")) proxy.flow = get("flow");
      if (get("encryption")) proxy.encryption = get("encryption");
      if (get("packetEncoding"))
        proxy["packet-encoding"] = get("packetEncoding");
      tlsSettings(proxy, get, ["tls", "reality"].includes(get("security")));
      transport(proxy, get);
    } else if (["trojan", "anytls", "hysteria2"].includes(type)) {
      proxy.password = password ? `${user}:${password}` : user;
      if (!proxy.password) throw new ShareLinkError();
      tlsSettings(proxy, get, true);
      if (type === "trojan") transport(proxy, get);
      if (type === "hysteria2") {
        if (get("obfs")) proxy.obfs = get("obfs");
        if (get("obfs-password")) proxy["obfs-password"] = get("obfs-password");
      }
    } else if (type === "ss") {
      const auth = password ? `${user}:${password}` : decode64(user);
      const separator = auth.indexOf(":");
      if (separator < 1) throw new ShareLinkError();
      proxy.cipher = auth.slice(0, separator);
      proxy.password = auth.slice(separator + 1);
      if (get("plugin")) {
        const [plugin, ...options] = get("plugin").split(";");
        const values = Object.fromEntries(
          options.map((option) => {
            const i = option.indexOf("=");
            return i < 0
              ? [option, true]
              : [option.slice(0, i), option.slice(i + 1)];
          }),
        );
        if (["obfs-local", "simple-obfs"].includes(plugin)) {
          proxy.plugin = "obfs";
          proxy["plugin-opts"] = {
            mode: values.obfs,
            host: values["obfs-host"],
          };
        } else if (plugin === "v2ray-plugin") {
          proxy.plugin = plugin;
          proxy["plugin-opts"] = values;
        } else throw new ShareLinkError();
      }
    } else if (type === "socks5" || type === "http" || type === "https") {
      if (type === "https") {
        proxy.type = "http";
        proxy.tls = true;
      }
      let auth = [user, password];
      if (user && !password) {
        const decoded = decode64(user),
          separator = decoded.indexOf(":");
        if (separator >= 0)
          auth = [decoded.slice(0, separator), decoded.slice(separator + 1)];
      }
      if (auth[0]) proxy.username = auth[0];
      if (auth[1]) proxy.password = auth[1];
      proxy.port = Number(
        url.port || (type === "https" ? 443 : type === "http" ? 80 : 0),
      );
    } else if (type === "tuic") {
      proxy.uuid = uuid(user);
      proxy.password = password;
      tlsSettings(proxy, get, true);
      for (const key of ["congestion-controller", "udp-relay-mode"])
        if (get(key)) proxy[key] = get(key);
    } else throw new ShareLinkError();
    return validate(proxy);
  } catch {
    throw new ShareLinkError();
  }
}
