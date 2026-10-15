#!/usr/bin/env python3
"""Build a SQLite fixture that both the Go and the archived Node service can read.

The two implementations must agree on the stored schema (they do - see
legacy/node-server/store.js and backend/internal/store/sqlite/sqlite.go), so a
single database is enough to diff the two HTTP surfaces with
backend/tools/api-parity.py.

Usage: python backend/tools/make-parity-fixture.py /tmp/parity/nodes.sqlite
"""
import json
import os
import sqlite3
import sys

SCHEMA = """
CREATE TABLE IF NOT EXISTS snapshots (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fingerprint TEXT NOT NULL UNIQUE,
  checked_at TEXT,
  imported_at TEXT NOT NULL,
  node_count INTEGER NOT NULL,
  payload TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ippure_results (
  snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
  node_index INTEGER NOT NULL, payload TEXT NOT NULL,
  PRIMARY KEY (snapshot_id, node_index)
);
CREATE TABLE IF NOT EXISTS retired_nodes (
  identity TEXT PRIMARY KEY,
  retired_at TEXT NOT NULL,
  reason TEXT NOT NULL
);
"""

COUNTRIES = ["US", "JP", "DE", "SG", "GB", "HK", "TW", "FR", "NL", "CA"]
TYPES = ["ss", "vless", "vmess", "trojan", "hysteria2"]


def node(i, batch):
    """One node with a realistic spread of filterable attributes.

    Key presence matters as much as the values: the JavaScript ingest copies a
    whitelist field only when the source has the key. This fixture uses a
    typical spread of source fields; the Go model also preserves explicit zero
    values and nulls when supplied by upstream JSON.
    """
    country = COUNTRIES[(i * 3 + batch) % len(COUNTRIES)]
    kind = TYPES[(i + batch) % len(TYPES)]
    server = "198.51.%d.%d" % (batch + 100, (i * 7) % 250 + 1)
    speed = 0 if i % 11 == 0 else (i * 337) % 9000 + 128
    out = {
        "name": "%s-%02d" % (country or "ZZ", i),
        "baseName": "node-%02d" % i,
        "type": kind,
        "server": server,
        "port": str(443 + (i % 5)),
        "sni": "%s.example.com" % kind,
        "tls": i % 4 != 0,
        "udp": i % 2 == 0,
        "network": ["tcp", "ws", "grpc", "h2"][i % 4],
        "speed": speed,
        "country": country,
        "ip": server,
        "ipRisk": ["low", "medium", "high"][i % 3],
        "subTag": "subs-check",
        "details": [{"k": "protocol", "v": kind}],
        "rawLink": "ss://YWVzLTI1Ni1nY206c2VjcmV0@%s:%d#%s" % (
            server, 443 + (i % 5), country or "ZZ"),
    }
    if i % 17 == 0:
        out["reality"] = True
    media = []
    if i % 3 == 0:
        media.append({"platform": "netflix", "tag": country})
    if i % 5 == 0:
        media.append({"platform": "youtube", "tag": country})
    if media:
        out["media"] = media
    # Nodes without an IPPure row omit country to exercise the "unknown" facet.
    if i % 3 == 2:
        del out["country"]
    return out


def snapshot(batch):
    nodes = [node(i, batch) for i in range(9 + batch)]
    return {
        "checkedAt": "2026-10-14T%02d:00:49.000Z" % (17 + batch),
        "speedTest": True,
        "mediaCheck": True,
        "nodes": nodes,
    }


def enrichment(batch, index):
    """Attach IPPure results to roughly two thirds of the rows."""
    if index % 3 == 2:
        return None
    out = {
        "checkedAt": "2026-10-14T%02d:01:00.000Z" % (17 + batch),
        "attempts": 1,
        "status": "success",
        "ip": "198.51.%d.%d" % (batch + 100, (index * 7) % 250 + 1),
        "asn": "AS%d" % (1000 + index),
        "asnOrganization": "Example ISP %d" % index,
        "source": ["ippure", "echo"][index % 2],
        "attributes": [["residential", "datacenter"], ["mobile", "native"]][index % 2],
        "humanPercent": 90.0 - index,
        "botPercent": 3.0 + index,
        "score": 88.0 - index,
        "label": ["clean", "neutral", "risky"][index % 3],
        "country": COUNTRIES[index % len(COUNTRIES)],
        "addressFamily": 4,
        "exitIPSource": "echo",
        "exitIPMode": "native",
        "exitIPVersion": 1,
        "exitIPProbeVersion": 2,
        "queryMode": "signed",
        "transportMode": "mihomo",
        "signatureVersion": 1,
    }
    # The collector only ever records the reachable case; a false value is never
    # written, so it must not appear in a fixture.
    if index % 2 == 0:
        out["githubReachable"] = True
    return out


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/parity/nodes.sqlite"
    if os.path.dirname(path):
        os.makedirs(os.path.dirname(path), exist_ok=True)
    if os.path.exists(path):
        os.remove(path)
    for suffix in ("-wal", "-shm"):
        if os.path.exists(path + suffix):
            os.remove(path + suffix)

    db = sqlite3.connect(path)
    db.executescript(SCHEMA)
    for batch in range(3):
        snap = snapshot(batch)
        db.execute(
            "INSERT INTO snapshots (fingerprint, checked_at, imported_at, node_count, payload)"
            " VALUES (?, ?, ?, ?, ?)",
            (
                "fixture-batch-%d" % batch,
                snap["checkedAt"],
                "2026-10-14T%02d:02:00.000Z" % (17 + batch),
                len(snap["nodes"]),
                json.dumps(snap, ensure_ascii=False, separators=(",", ":")),
            ),
        )
        snapshot_id = db.execute("SELECT last_insert_rowid()").fetchone()[0]
        for index in range(len(snap["nodes"])):
            payload = enrichment(batch, index)
            if payload is None:
                continue
            db.execute(
                "INSERT INTO ippure_results (snapshot_id, node_index, payload) VALUES (?, ?, ?)",
                (snapshot_id, index, json.dumps(payload, ensure_ascii=False, separators=(",", ":"))),
            )
    db.commit()
    rows = db.execute("SELECT id, node_count FROM snapshots ORDER BY id").fetchall()
    enrich = db.execute("SELECT count(*) FROM ippure_results").fetchone()[0]
    db.close()
    print("fixture %s" % path)
    print("  snapshots: %s" % rows)
    print("  enrichments: %d" % enrich)


if __name__ == "__main__":
    main()
