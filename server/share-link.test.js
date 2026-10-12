import { test } from "node:test";
import assert from "node:assert/strict";
import { shareLinkProxy } from "./share-link.js";

const id = "12345678-1234-1234-1234-123456789abc";
const base64 = (value) => Buffer.from(value).toString("base64url");

test("VLESS preserves credentials, Reality, gRPC and packet encoding", () => {
  const proxy = shareLinkProxy(
    `vless://${id}@example.com:443?security=reality&sni=tls.example&pbk=public&sid=abcd&fp=chrome&flow=xtls-rprx-vision&type=grpc&serviceName=service&packetEncoding=xudp`,
    "Node",
  );
  assert.equal(proxy.uuid, id);
  assert.equal(proxy.servername, "tls.example");
  assert.equal(proxy.tls, true);
  assert.equal(proxy.flow, "xtls-rprx-vision");
  assert.equal(proxy["packet-encoding"], "xudp");
  assert.deepEqual(proxy["reality-opts"], {
    "public-key": "public",
    "short-id": "abcd",
  });
  assert.deepEqual(proxy["grpc-opts"], { "grpc-service-name": "service" });
});

test("WebSocket, HTTP upgrade, XHTTP and HTML-escaped links preserve transport options", () => {
  const ws = shareLinkProxy(
    `vless://${id}@[2001:db8::1]:443?security=tls&amp;type=ws&amp;host=ws.example&amp;path=%2Fsocket&ed=2048&eh=Sec-WebSocket-Protocol`,
  );
  assert.equal(ws.server, "2001:db8::1");
  assert.deepEqual(ws["ws-opts"], {
    path: "/socket",
    headers: { Host: "ws.example" },
    "max-early-data": 2048,
    "early-data-header-name": "Sec-WebSocket-Protocol",
  });
  const upgrade = shareLinkProxy(
    `vless://${id}@example.com:443?type=httpupgrade&path=%2Fupgrade`,
  );
  assert.equal(upgrade["ws-opts"]["v2ray-http-upgrade"], true);
  const xhttp = shareLinkProxy(
    `vless://${id}@example.com:443?type=xhttp&mode=packet-up&path=%2Fstream&extra=${encodeURIComponent(JSON.stringify({ noGRPCHeader: true }))}`,
  );
  assert.deepEqual(xhttp["xhttp-opts"], {
    path: "/stream",
    mode: "packet-up",
    "no-grpc-header": true,
  });
});

test("VMess JSON links preserve cipher, alterId, TLS, and WebSocket configuration", () => {
  const proxy = shareLinkProxy(
    `vmess://${base64(JSON.stringify({ add: "example.com", port: "443", id, aid: "0", scy: "auto", net: "ws", tls: "tls", host: "ws.example", sni: "tls.example", path: "/ws" }))}`,
  );
  assert.equal(proxy.uuid, id);
  assert.equal(proxy.alterId, 0);
  assert.equal(proxy.cipher, "auto");
  assert.equal(proxy.servername, "tls.example");
  assert.deepEqual(proxy["ws-opts"], {
    path: "/ws",
    headers: { Host: "ws.example" },
  });
});

test("Shadowsocks SIP002 and full base64 links preserve passwords containing colons", () => {
  for (const link of [
    `ss://${base64("aes-256-gcm:secret:with:colon")}@example.com:8388#Node`,
    `ss://${base64("aes-256-gcm:secret:with:colon@example.com:8388")}#Node`,
    "ss://aes-256-gcm:secret%3Awith%3Acolon@example.com:8388#Node",
  ]) {
    const proxy = shareLinkProxy(link);
    assert.equal(proxy.cipher, "aes-256-gcm");
    assert.equal(proxy.password, "secret:with:colon");
    assert.equal(proxy.port, 8388);
  }
});

test("SSR decodes protocol and obfuscation credentials", () => {
  const proxy = shareLinkProxy(
    `ssr://${base64(`example.com:8388:auth_aes128_md5:aes-256-cfb:tls1.2_ticket_auth:${base64("secret")}/?obfsparam=${base64("tls.example")}&protoparam=${base64("parameter")}`)}`,
  );
  assert.equal(proxy.type, "ssr");
  assert.equal(proxy.password, "secret");
  assert.equal(proxy["obfs-param"], "tls.example");
  assert.equal(proxy["protocol-param"], "parameter");
});

test("Trojan, Hysteria2, AnyTLS, TUIC and SOCKS links preserve authentication", () => {
  for (const scheme of ["trojan", "hy2", "hysteria2", "anytls"]) {
    const proxy = shareLinkProxy(
      `${scheme}://secret%3Avalue@example.com:443?sni=tls.example&insecure=1&alpn=h3,h2`,
    );
    assert.equal(proxy.password, "secret:value");
    assert.equal(proxy.sni, "tls.example");
    assert.equal(proxy["skip-cert-verify"], true);
    assert.deepEqual(proxy.alpn, ["h3", "h2"]);
  }
  const hy2 = shareLinkProxy(
    "hy2://secret@example.com:443?obfs=salamander&obfs-password=obfs-secret",
  );
  assert.equal(hy2["obfs-password"], "obfs-secret");
  const tuic = shareLinkProxy(
    `tuic://${id}:secret@example.com:443?congestion-controller=bbr&udp-relay-mode=native`,
  );
  assert.equal(tuic.uuid, id);
  assert.equal(tuic.password, "secret");
  assert.equal(tuic["congestion-controller"], "bbr");
  for (const link of [
    "socks5://user:secret@example.com:1080",
    `socks5://${base64("user:secret")}@example.com:1080`,
  ]) {
    const proxy = shareLinkProxy(link);
    assert.equal(proxy.username, "user");
    assert.equal(proxy.password, "secret");
  }
});

test("unsupported and malformed links fail without leaking source credentials", () => {
  for (const link of [
    "vless://secret@example.com:443",
    `vless://${id}@example.com:443?type=unknown`,
    `vless://${id}@example.com:443?security=reality`,
    "hy2://secret@example.com:443?sni=bad%00host",
    "unknown://private@example.com:443",
    "vmess://invalid",
  ]) {
    assert.throws(
      () => shareLinkProxy(link),
      (error) => {
        assert.equal(
          error.message,
          "节点分享链接格式或连接参数不受当前检测服务支持",
        );
        assert.equal(error.message.includes("private"), false);
        return true;
      },
    );
  }
});
