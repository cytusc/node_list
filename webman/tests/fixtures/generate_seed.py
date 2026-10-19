import json

def j(o):
    s = json.dumps(o, ensure_ascii=False, separators=(',', ':'))
    return "$json$" + s + "$json$"

nodes = []

n1 = {
    "name": "\U0001F1FA\U0001F1F8 美国 01", "baseName": "美国 01", "type": "ss",
    "server": "198.51.100.10", "port": 443, "sni": "us01.example.com",
    "tls": True, "reality": False, "udp": True, "network": "tcp", "speed": 5242880,
    "country": "US",
    "media": [{"platform": "Netflix", "tag": "解锁"}, {"platform": "YouTube", "tag": "解锁"}],
    "details": [{"k": "加密", "v": "aes-256-gcm"}],
    "speedTest": True, "mediaCheck": True, "checkedAt": "2026-10-18T00:00:00Z",
    "rawLink": "ss://secret",
    "rawConfig": {"type": "ss", "server": "198.51.100.10", "port": 443},
}
nodes.append(("id-us-ss", 1, 0, n1,
    {"reachable": True, "checkedAt": "__NOW2M__", "error": ""},
    {"ip": "203.0.113.10", "addressFamily": 4, "country": "US", "checkedAt": "__NOW2M__",
     "source": "ip.sb", "error": "", "version": 3},
    {"ip": "203.0.113.10", "status": "success", "source": "ippure",
     "asnOrganization": "AS15169 Google LLC", "score": 85, "humanPercent": 92.5,
     "attributes": ["residential", "isp"], "country": "US", "checkedAt": "__NOW2M__",
     "classification": {"version": 8, "ip": "203.0.113.10", "checkedAt": "__NOW2M__",
        "state": "residential",
        "types": [{"provider": "ippure", "usageType": "住宅IP", "companyType": "商业宽带"}],
        "evidence": [{"provider": "ippure", "checkedAt": "__NOW2M__", "ip": "203.0.113.10",
                      "hosting": False, "vpn": False, "proxy": False,
                      "outcome": "supports-residential"}]}},
    True, False))

n2 = {
    "name": "\U0001F1EF\U0001F1F5 日本 10", "baseName": "日本 10", "type": "vmess",
    "server": "198.51.100.20", "port": "8443", "sni": "jp10.example.com",
    "tls": True, "reality": False, "udp": False, "network": "ws", "speed": 10485760,
    "country": "JP", "media": [{"Platform": "Netflix", "Tag": "解锁"}],
    "details": [], "speedTest": True, "mediaCheck": True, "checkedAt": "2026-10-18T00:00:00Z",
}
nodes.append(("id-jp-vmess", 1, 1, n2,
    {"reachable": True, "checkedAt": "__NOW2M__", "error": ""},
    {"ip": "203.0.113.20", "addressFamily": 4, "country": "JP", "checkedAt": "__NOW2M__",
     "source": "ipinfo", "error": "", "version": 3},
    {"ip": "203.0.113.20", "status": "success", "source": "ipinfo",
     "asnOrganization": "AS2516 KDDI", "score": 40, "humanPercent": 61,
     "attributes": ["business"], "country": "JP", "checkedAt": "__NOW2M__",
     "classification": {"version": 8, "ip": "203.0.113.20", "checkedAt": "__NOW2M__",
        "state": "business",
        "types": [{"provider": "ipinfo", "usageType": "商业宽带"}],
        "evidence": [{"provider": "ipinfo", "checkedAt": "__NOW2M__", "ip": "203.0.113.20",
                      "hosting": False, "vpn": False, "proxy": False,
                      "outcome": "non-hosting", "httpStatus": 200}]}},
    True, False))

n3 = {
    "name": "\U0001F1E9\U0001F1EA 德国 02", "baseName": "德国 02", "type": "trojan",
    "server": "198.51.100.30", "port": 443, "sni": "de02.example.com",
    "tls": True, "reality": True, "udp": True, "network": "tcp", "speed": 0,
    "country": "DE", "media": [], "details": [{"k": "流量", "v": "1T"}],
    "speedTest": True, "mediaCheck": True, "checkedAt": "2026-10-18T00:00:00Z",
}
nodes.append(("id-de-trojan", 2, 0, n3, None, None, None, True, False))

n4 = {
    "name": "\U0001F1F8\U0001F1EC 新加坡 01", "baseName": "新加坡 01", "type": "hysteria2",
    "server": "198.51.100.40", "port": 2096, "tls": True, "udp": True, "network": "quic",
    "speed": 2097152, "country": "SG", "ipRisk": "low",
    "media": [{"platform": "Disney+", "tag": "解锁"}],
    "speedTest": True, "mediaCheck": True, "checkedAt": "2026-10-18T00:00:00Z",
}
nodes.append(("id-sg-hy2", 2, 1, n4,
    {"reachable": False, "checkedAt": "__NOW2M__", "error": "dial timeout"},
    {"ip": "203.0.113.40", "addressFamily": 6, "country": "SG", "checkedAt": "__NOW2M__",
     "source": "ip.sb", "error": "", "version": 3},
    None, True, False))

n5 = {
    "name": "\U0001F1ED\U0001F1F0 香港 03", "baseName": "香港 03", "type": "ss",
    "server": "198.51.100.10", "port": 8443, "tls": False, "udp": True, "network": "tcp",
    "speed": 7340032, "country": "",
    "media": [{"platform": "Netflix", "tag": "限自制剧"}],
    "speedTest": True, "mediaCheck": True, "checkedAt": "2026-10-18T00:00:00Z",
}
nodes.append(("id-hk-ss", 2, 2, n5, None, None, None, True, False))

n6 = {
    "name": "\U0001F1FA\U0001F1F8 美国 99", "baseName": "美国 99", "type": "ss",
    "server": "198.51.100.99", "port": 443, "tls": True, "udp": True, "network": "tcp",
    "speed": 1048576, "country": "US", "speedTest": True, "mediaCheck": True,
    "checkedAt": "2026-10-18T00:00:00Z",
}
nodes.append(("id-us-retired", 2, 3, n6, None, None, None, True, False))

n7 = {
    "name": "\U0001F1EC\U0001F1E7 英国 07", "baseName": "英国 07", "type": "vmess",
    "server": "198.51.100.70", "port": 443, "tls": True, "udp": False, "network": "ws",
    "speed": 3145728, "country": "GB", "speedTest": True, "mediaCheck": True,
    "checkedAt": "2026-10-18T00:00:00Z",
}
nodes.append(("id-gb-unavail", 2, 4, n7, None, None, None, False, False))

out = ["BEGIN;"]
out.append(
    "TRUNCATE node_explorer_snapshots, node_explorer_retired_nodes, "
    "node_explorer_ippure_results, node_explorer_node_registry, "
    "node_explorer_connectivity, node_explorer_exits, node_explorer_ip_attributes, "
    "node_explorer_work, node_explorer_recovery_events, node_explorer_recovery_totals, node_explorer_service_status, node_explorer_lifecycle_events "
    "RESTART IDENTITY CASCADE;")

for sid, fp, ca in [(1, "fixture-1", "2026-10-17T23:00:00Z"),
                    (2, "fixture-2", "2026-10-18T00:00:00Z")]:
    out.append(
        "INSERT INTO node_explorer_snapshots "
        "(id,fingerprint,checked_at,imported_at,node_count,payload) VALUES "
        "(%d,'%s','%s','%s',0,'{}');" % (sid, fp, ca, ca))
out.append("SELECT setval('node_explorer_snapshots_id_seq', 2, true);")

for (ident, sid, idx, node, conn, exitp, attr, avail, hist) in nodes:
    out.append(
        "INSERT INTO node_explorer_node_registry "
        "(identity,node,snapshot_id,node_index,historical_residential,available) VALUES "
        "('%s',%s,%d,%d,%s,%s);"
        % (ident, j(node), sid, idx, str(hist).lower(), str(avail).lower()))
    if conn:
        out.append(
            "INSERT INTO node_explorer_connectivity (identity,checked_at,payload) VALUES "
            "('%s', now()-interval '2 minutes', %s);" % (ident, j(conn)))
    if exitp:
        # payload 必须保留 ip：Go 侧 probes.go 是把整个出口验证结构体
        # （含 ip）序列化进 node_explorer_exits.payload 的，而 live_facts 的
        # reported_exit_ip 取自 exit_payload->>'ip'（不是 e.ip 列）。
        # 只有 version 要剔掉——它是表列，由 live_nodes 视图回填。
        payload = {k: v for k, v in exitp.items() if k != "version"}
        out.append(
            "INSERT INTO node_explorer_exits (identity,ip,version,checked_at,payload) VALUES "
            "('%s','%s',%d,now()-interval '2 minutes',%s);"
            % (ident, exitp["ip"], exitp["version"], j(payload)))
    if attr:
        out.append(
            "INSERT INTO node_explorer_ip_attributes (ip,checked_at,payload) VALUES "
            "('%s',now()-interval '2 minutes',%s);" % (attr["ip"], j(attr)))

out.append(
    "INSERT INTO node_explorer_retired_nodes (identity,retired_at,reason) VALUES "
    "('id-us-retired','2026-10-17T20:00:00Z','github-unreachable');")
out.append("COMMIT;")

body = "\n".join(out)

header = (
    "-- 集成测试数据：覆盖排序、筛选、facets、优选位、退役排除、不可用排除、IPv6 出口。\n"
    "-- 由 tests/fixtures/generate_seed.py 生成，不要手工编辑。\n"
    "-- 时间占位 __NOW2M__ 由 tests/fixtures/load.sh 在导入前替换成真实时间戳\n"
    "-- （优选位判定要求连通性/出口证据在 10 分钟内，固定时间会随时间失效）。\n")
with open("tests/fixtures/seed.sql", "w", encoding="utf-8", newline="\n") as f:
    f.write(header + body + "\n")
print("ok")
