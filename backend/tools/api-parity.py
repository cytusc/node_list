#!/usr/bin/env python3
"""Diff the Go API against the archived Node API across many query shapes.

This is the cutover gate: the Go service replaced the JavaScript one, and the
frontend plus the deployment tooling depend on the exact status codes, error
messages and JSON key sets it produced. Byte-identical responses are the
acceptance criterion, so keep this passing whenever either side is touched.

Usage
-----
1. Start the archived JavaScript service against a fixture database::

       PGHOST=... DATABASE_PATH=/tmp/parity/nodes.sqlite API_PORT=3011 \
         node legacy/node-server/index.js

2. Start the Go service against the same database::

       PGHOST=... DATABASE_PATH=/tmp/parity/nodes.sqlite API_PORT=3012 \
         backend/node-explorer

3. Compare::

       python backend/tools/api-parity.py

Both services must read the *same* database, otherwise /api/history and the
snapshot ids legitimately differ. Override the endpoints with GO_BASE and
NODE_BASE when they run elsewhere.
"""
import json
import os
import sys
import urllib.error
import urllib.request

GO = os.environ.get("GO_BASE", "http://127.0.0.1:3012")
NODE = os.environ.get("NODE_BASE", "http://127.0.0.1:3011")

CASES = [
    "/api/results",
    "/api/results?page=1&limit=5",
    "/api/results?page=2&limit=3",
    "/api/results?page=1&limit=5&sort=speed&direction=asc",
    "/api/results?page=1&limit=5&sort=country&direction=asc",
    "/api/results?page=1&limit=5&sort=trafficRatio&direction=desc",
    "/api/results?page=1&limit=5&q=us",
    "/api/results?page=1&limit=5&country=US&type=vless",
    "/api/results?page=1&limit=5&features=tls,udp",
    "/api/results?page=1&limit=5&network=ws",
    "/api/results?page=1&limit=5&minSpeed=1",
    "/api/results?page=1&limit=200",
    "/api/results?page=99&limit=5",
    "/api/results?snapshot=1",
    "/api/results?snapshot=99999",
    "/api/results?view=aggregate",
    "/api/results?view=aggregate&page=1&limit=5",
    "/api/results?page=0",
    "/api/results?limit=999",
    "/api/results?sort=bogus",
    "/api/results?direction=sideways",
    "/api/results?view=bogus",
    "/api/results?view=aggregate&snapshot=1",
    "/api/results?features=bogus",
    "/api/history",
    "/api/health",
    "/api/nope",
]


def fetch(base, path):
    try:
        with urllib.request.urlopen(base + path, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8")
        try:
            return exc.code, json.loads(body)
        except Exception:
            return exc.code, body


def diff(a, b, out):
    if a == b:
        return
    if isinstance(a, dict) and isinstance(b, dict):
        for key in sorted(set(a) | set(b)):
            if key in ("nodes", "facets"):
                continue
            if a.get(key) != b.get(key):
                out.append("    field %s: go=%r node=%r" % (
                    key, str(a.get(key))[:120], str(b.get(key))[:120]))
        af, bf = a.get("facets"), b.get("facets")
        if af != bf and isinstance(af, dict) and isinstance(bf, dict):
            for name in sorted(set(af) | set(bf)):
                if af.get(name) != bf.get(name):
                    out.append("    facet %s: go=%s node=%s" % (
                        name, json.dumps(af.get(name), ensure_ascii=False)[:220],
                        json.dumps(bf.get(name), ensure_ascii=False)[:220]))
                    break
            out.append("    facets differ")
        an, bn = a.get("nodes"), b.get("nodes")
        if an != bn and isinstance(an, list) and isinstance(bn, list):
            if len(an) != len(bn):
                out.append("    nodes len: go=%d node=%d" % (len(an), len(bn)))
            for i, (x, y) in enumerate(zip(an, bn)):
                if x != y:
                    out.append("    node[%d] go=%s" % (i, json.dumps(x, ensure_ascii=False)[:200]))
                    out.append("    node[%d] node=%s" % (i, json.dumps(y, ensure_ascii=False)[:200]))
                    break
    else:
        out.append("    go  : %s" % json.dumps(a, ensure_ascii=False)[:300])
        out.append("    node: %s" % json.dumps(b, ensure_ascii=False)[:300])


def main():
    failures = 0
    for path in CASES:
        gs, g = fetch(GO, path)
        ns, n = fetch(NODE, path)
        out = []
        if gs != ns:
            out.append("    status: go=%d node=%d" % (gs, ns))
        diff(g, n, out)
        if out:
            failures += 1
            print("FAIL %s" % path)
            for line in out[:6]:
                print(line)
        else:
            print("ok   %s (status=%d)" % (path, gs))
    print("\n%d/%d cases identical" % (len(CASES) - failures, len(CASES)))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
