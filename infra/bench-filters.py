#!/usr/bin/env python3
"""
Filter-matrix benchmark: every filter combination, on both search engines.

Compares the composite GSI path against the Search (FTS) path for the same
filters, by calling the pipeline's own /notifications endpoint - so the numbers
include the handler, not just the engine. The FTS arm is pinned with
`engine=fts`; without it the server routes keyword filters to the GSI.

READ-ONLY with respect to data and configuration. Every measurement is a GET
against the search endpoint; the script creates no documents, builds no indexes
and changes no settings. The one write it issues is DELETE FROM
system:active_requests, which cancels a running query and touches no data.

Timeout policy (as specified):

  * Each query gets 5s. Exceeding it is a result, not an error - recorded as
    TIMEOUT and the request is abandoned.
  * If BOTH engines exceed 5s for the same filters, both are re-run with a 60s
    ceiling so the slower path is still identified.
  * Nothing is ever allowed to run past 60s.

Timeouts are enforced ON THE SERVER, not just in this client. Abandoning an HTTP
read does not stop the query behind it - an earlier version of this script left
queries running for over a minute after it had moved on, so later rows were
measured against a cluster still busy with earlier ones. On every timeout the
script now cancels the underlying query and waits for the engine to go idle
before continuing, so exactly one query is ever in flight:

  * N1QL  DELETE FROM system:active_requests WHERE requestId = "..."
  * FTS   POST /api/query/{id}/cancel  {"uuid": "<node-uuid>"}

Only requests this script started are cancelled: the active set is snapshotted
before each query and anything already running is left alone, as is the
pipeline's own retry scheduler (matched on delivery.next_attempt_at).

Usage:

    ./bench-filters.py                          # everything, CSV next to the script
    ./bench-filters.py --phase sweep            # dropdown values only
    ./bench-filters.py --phase matrix           # field combinations only
    ./bench-filters.py --runs 3                 # best-of-3 per cell
    ./bench-filters.py --limit 20 --dry-run     # see what would run

    BASE=http://<app-host>:8080 CB=http://<cb-host>:8091 ./bench-filters.py
"""

import argparse
import base64
import csv
import itertools
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

DEFAULT_BASE = os.environ.get("BASE", "http://localhost:8080")
BASE = DEFAULT_BASE
FAST_TIMEOUT = 5.0      # the stated limit
SLOW_TIMEOUT = 60.0     # only for combinations where BOTH engines blew the limit

# ---------------------------------------------------------------------------
# Field values. These are the real values in the dataset, not invented ones:
# channels and statuses come from types.go, app names from loadgen.go, types
# from the seeded event_defs, and the user id shape from live documents.
# ---------------------------------------------------------------------------

CHANNELS = ["email", "sms", "push", "cblite"]
STATUSES = ["PENDING", "DELIVERED", "FAILED"]
APP_NAMES = ["ncgrdemo", "portal", "hr_self_service", "procurement", "payroll", "licensing"]
TYPES = [
    "account_alert", "appointment_reminder", "document_expiry", "invoice_ready",
    "otp_login", "payment_received", "reset_password", "service_request_update",
]
SEEN = ["true", "false"]

# A user that actually exists. The load generator draws from a pool of 100k with
# this shape; a user id matching nothing would measure the empty-result path and
# flatter both engines.
SAMPLE_USER = os.environ.get("SAMPLE_USER", "usr_055270")

# Message text for the text-search phase. The GSI cannot serve text at all, so
# those rows are FTS-only by construction - see run_text_phase.
SAMPLE_TEXT = os.environ.get("SAMPLE_TEXT", "password")


def time_windows():
    """The two requested ranges. 'Last 30 days' is relative to the run."""
    now = datetime.now(timezone.utc)
    return {
        "last30d": {
            "from": (now - timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "to": now.strftime("%Y-%m-%dT%H:%M:%SZ"),
        },
        "oct2025": {"from": "2025-10-01T00:00:00Z", "to": "2025-10-31T23:59:59Z"},
    }


# Representative value per field for the combination matrix. Every value of the
# dropdown fields is covered separately by the sweep phase; repeating that inside
# the matrix would multiply 127 combinations by several hundred.
REPRESENTATIVE = {
    "user_id": SAMPLE_USER,
    "app_name": "ncgrdemo",
    "channel": "email",
    "status": "PENDING",
    "type": "otp_login",
    "seen": "false",
}
MATRIX_FIELDS = ["time", "user_id", "app_name", "channel", "status", "type", "seen"]


# ---------------------------------------------------------------------------
# Cluster access - used only to cancel queries this script started
# ---------------------------------------------------------------------------

DEFAULT_CB = os.environ.get("CB", "http://localhost:8091")
CB = DEFAULT_CB
CB_AUTH = (os.environ.get("CB_USER", "Administrator"), os.environ.get("CB_PASS", "password"))

# The pipeline's retry scheduler queries `notifications` on a timer. It is not
# ours and must never be cancelled; it is the only other N1QL traffic against
# this keyspace and is identifiable by the retry index's key.
SCHEDULER_MARKER = "next_attempt_at"


def _cb_request(path, data=None, headers=None, timeout=20):
    url = f"{CB}{path}"
    req = urllib.request.Request(url, data=data, headers=headers or {})
    token = base64.b64encode(f"{CB_AUTH[0]}:{CB_AUTH[1]}".encode()).decode()
    req.add_header("Authorization", "Basic " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.load(resp)
    except Exception:
        return {}


def _n1ql(statement, timeout=20):
    body = urllib.parse.urlencode({"statement": statement}).encode()
    return _cb_request("/_p/query/query/service", data=body,
                       headers={"Content-Type": "application/x-www-form-urlencoded"},
                       timeout=timeout)


def active_n1ql():
    """requestIds of running notification queries, excluding ours and the scheduler."""
    res = _n1ql("SELECT requestId, statement FROM system:active_requests")
    out = set()
    for row in res.get("results", []):
        stmt = row.get("statement") or ""
        if "active_requests" in stmt or SCHEDULER_MARKER in stmt:
            continue
        if "notifications" in stmt:
            out.add(row["requestId"])
    return out


def active_fts():
    """Active FTS query ids, keyed '<node-uuid>-<n>'."""
    res = _cb_request("/_p/fts/api/query")
    return set((res.get("filteredActiveQueries") or {}).get("queryMap", {}).keys())


def cancel_n1ql(request_ids):
    for rid in request_ids:
        _n1ql(f'DELETE FROM system:active_requests WHERE requestId = "{rid}"')


def cancel_fts(query_ids):
    for qid in query_ids:
        node, _, num = qid.rpartition("-")
        if not node or not num.isdigit():
            continue
        _cb_request(f"/_p/fts/api/query/{num}/cancel",
                    data=json.dumps({"uuid": node}).encode(),
                    headers={"Content-Type": "application/json"})


def cancel_ours(engine, before_n1ql, before_fts):
    """
    Cancel whatever this cell started, and wait for the engine to go quiet.
    Returns True if it went idle, False if something was still running - which
    would mean the next row is measured against a busy cluster and is reported.
    """
    if engine == "fts":
        cancel_fts(active_fts() - before_fts)
    else:
        cancel_n1ql(active_n1ql() - before_n1ql)

    deadline = time.perf_counter() + 15
    while time.perf_counter() < deadline:
        leftover = (active_fts() - before_fts) if engine == "fts" else (active_n1ql() - before_n1ql)
        if not leftover:
            return True
        time.sleep(0.5)
    return False


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

def query(filters, engine, timeout):
    """
    One search. Returns (outcome, server_ms, wall_ms, rows, total).

    outcome is "ok", "timeout" or "error:<detail>". server_ms is the handler's
    own measurement (tookMs) and is the number to compare - wall_ms includes
    network and is reported only to show what a human would have waited.
    """
    params = dict(filters)
    if engine == "fts":
        params["engine"] = "fts"
    if not params:
        params["x"] = "1"  # an empty query string is the unfiltered feed
    url = f"{BASE}/notifications?" + urllib.parse.urlencode(params)

    # Snapshot what is already running so the cancel only ever targets this cell.
    before_n1ql = active_n1ql() if engine == "gsi" else set()
    before_fts = active_fts() if engine == "fts" else set()

    started = time.perf_counter()
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            body = json.load(resp)
        wall = (time.perf_counter() - started) * 1000
        return ("ok", body.get("tookMs"), wall, len(body.get("rows") or []), body.get("total"))
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        wall = (time.perf_counter() - started) * 1000
        reason = getattr(exc, "reason", exc)
        if isinstance(reason, (TimeoutError, OSError)) or "timed out" in str(reason).lower():
            idle = cancel_ours(engine, before_n1ql, before_fts)
            return ("timeout" if idle else "timeout-uncancelled", None, wall, 0, None)
        return (f"error:{reason}", None, wall, 0, None)
    except json.JSONDecodeError as exc:
        wall = (time.perf_counter() - started) * 1000
        return (f"error:bad-json:{exc}", None, wall, 0, None)


def best_of(filters, engine, timeout, runs):
    """Fastest of N attempts. A timeout on any attempt ends the cell."""
    best = None
    for _ in range(max(1, runs)):
        outcome, server_ms, wall_ms, rows, total = query(filters, engine, timeout)
        if outcome != "ok":
            return (outcome, server_ms, wall_ms, rows, total)
        if best is None or (server_ms or wall_ms) < (best[1] or best[2]):
            best = (outcome, server_ms, wall_ms, rows, total)
    return best


def diagnose(gsi, fts):
    """
    Why is a cell slow? The two engines fail for opposite reasons, and the hit
    count separates them:

      * FTS cost tracks the NUMBER OF MATCHES - it ranks and sorts every match
        before returning a page, so a filter matching 150M documents is slow
        however selective it looks.
      * GSI cost tracks HOW DEEP the first matches sit in sort_key order - it
        walks the index in order and post-filters, so a rare value concentrated
        in old documents means walking everything newer first.
    """
    g_ok, f_ok = gsi[0] == "ok", fts[0] == "ok"
    total = fts[4] if isinstance(fts[4], int) else None
    if g_ok and f_ok:
        return ""
    if not g_ok and not f_ok:
        return f"both slow; FTS matches={total:,}" if total else "both slow"
    if not g_ok:
        return "GSI slow: matching rows sit deep in sort_key order (post-filter walk)"
    return f"FTS slow: ranks {total:,} matches before paging" if total else "FTS slow: high match count"



# ---------------------------------------------------------------------------
# Direct N1QL, for comparing a specific index (--gsi-index)
#
# The app has no "use this index" parameter and must not be changed for a test,
# so this arm talks to the query service directly. The statement is a faithful
# copy of db.go buildFeedQuery - same predicates, same ORDER BY, same LIMIT -
# with a USE INDEX hint added. Times here are query-service elapsedTime and so
# exclude the handler; they are compared against a control arm measured exactly
# the same way, never against the app-measured numbers from the other mode.
# ---------------------------------------------------------------------------

B32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"   # Crockford, must match util.go


def _encode_time(ms, n=10):
    out = [""] * n
    for i in range(n - 1, -1, -1):
        out[i] = B32[ms % 32]
        ms //= 32
    return "".join(out)


def ulid_floor(ms):
    return _encode_time(ms) + "0" * 16


def ulid_ceil(ms):
    return _encode_time(ms) + "Z" * 16


def _iso_to_ms(iso):
    return int(datetime.strptime(iso, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp() * 1000)


def build_n1ql(filters, index_name=None, limit=100):
    """
    Rebuild the handler's feed query, optionally pinned to one index.

    Uses NAMED PARAMETERS, not inlined literals, because db.go runs this through
    gocb with named parameters and Adhoc:false. That is not cosmetic: with
    literals the optimizer costs each value and - now that an adaptive index
    exists - picks an IntersectScan over idx_multi + adaptive_idx that times out
    on queries the app itself still answers in 5ms. Inlining would have measured
    a plan production never runs.
    """
    where = ["n.tenant_id = $tenant"]
    params = {"$tenant": '"ncgr"'}
    for key, param in (("user_id", "user"), ("app_name", "app"), ("channel", "chan"),
                       ("status", "status"), ("type", "ntype")):
        if filters.get(key):
            column = key
            where.append(f"n.{column} = ${param}")
            params[f"${param}"] = json.dumps(filters[key])
    if filters.get("seen") in ("true", "false"):
        where.append("n.seen = $seen")
        params["$seen"] = filters["seen"]
    # A time bound is a sort_key bound - the ULID prefix encodes the timestamp.
    if filters.get("from"):
        where.append("n.sort_key >= $from")
        params["$from"] = json.dumps(ulid_floor(_iso_to_ms(filters["from"])))
    if filters.get("to"):
        where.append("n.sort_key <= $to")
        params["$to"] = json.dumps(ulid_ceil(_iso_to_ms(filters["to"])))
    params["$limit"] = str(limit)

    hint = f" USE INDEX ({index_name} USING GSI)" if index_name else ""
    stmt = ("SELECT n.*\nFROM `ncgr`.`platform`.`notifications` AS n" + hint +
            "\nWHERE " + "\n  AND ".join(where) +
            "\nORDER BY n.sort_key DESC\nLIMIT $limit")
    return stmt, params


def run_n1ql(built, timeout):
    """
    Execute with the query service's OWN timeout, so it is cancelled server-side
    at the limit rather than abandoned. Returns (outcome, server_ms, wall_ms,
    rows, None).
    """
    stmt, params = built
    before = active_n1ql()
    form = {"statement": stmt, "timeout": f"{int(timeout)}s"}
    form.update(params)
    body = urllib.parse.urlencode(form).encode()
    started = time.perf_counter()
    res = _cb_request("/_p/query/query/service", data=body,
                      headers={"Content-Type": "application/x-www-form-urlencoded"},
                      timeout=timeout + 10)
    wall = (time.perf_counter() - started) * 1000

    if not res:
        cancel_ours("gsi", before, set())
        return ("timeout", None, wall, 0, None)
    errors = res.get("errors") or []
    # "Index scan timed out" is the indexer's own limit firing - the same
    # outcome as the query service's, reported by a different component.
    if any(("timeout" in str(e.get("msg", "")).lower()
            or "timed out" in str(e.get("msg", "")).lower()) for e in errors):
        cancel_ours("gsi", before, set())   # belt and braces; it should be gone
        return ("timeout", None, wall, 0, None)
    if errors:
        return (f"error:{errors[0].get('msg', '')[:60]}", None, wall, 0, None)
    elapsed = res.get("metrics", {}).get("elapsedTime", "")
    ms = None
    if elapsed.endswith("ms"):
        ms = float(elapsed[:-2])
    elif elapsed.endswith("s"):
        ms = float(elapsed[:-1]) * 1000
    elif elapsed.endswith("µs"):
        ms = float(elapsed[:-2]) / 1000
    return ("ok", ms, wall, res.get("metrics", {}).get("resultCount", 0), None)


def explain_index(built):
    """
    Which index(es) the plan really uses, whether it must sort, and whether the
    USE INDEX hint was honoured. A hint the optimizer declines is the difference
    between measuring the index you asked for and measuring a different one.
    """
    stmt, params = built
    form = {"statement": "EXPLAIN " + stmt}
    form.update(params)
    res = _cb_request("/_p/query/query/service", data=urllib.parse.urlencode(form).encode(),
                      headers={"Content-Type": "application/x-www-form-urlencoded"}, timeout=40)
    if not res.get("results"):
        return ("?", False, "?")
    result = res["results"][0]
    indexes, has_order = [], False

    def walk(node):
        nonlocal has_order
        if isinstance(node, dict):
            op = node.get("#operator")
            if op == "IndexScan3" and node.get("index"):
                indexes.append(node["index"])
            if op in ("Order", "OrderedIntersectScan"):
                has_order = True
            for value in node.values():
                walk(value)
        elif isinstance(node, list):
            for value in node:
                walk(value)

    walk(result.get("plan", {}))
    hints = result.get("optimizer_hints", {}) or {}
    if hints.get("hints_not_followed"):
        honoured = "NOT-FOLLOWED"
    elif hints.get("hints_followed"):
        honoured = "followed"
    else:
        honoured = "none"
    seen, ordered = set(), []
    for name in indexes:
        if name not in seen:
            seen.add(name); ordered.append(name)
    return ("+".join(ordered) or "?", has_order, honoured)


# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------

def sweep_cases(windows):
    """Every value of every dropdown field, one field at a time."""
    for value in CHANNELS:
        yield (f"channel={value}", {"channel": value})
    for value in STATUSES:
        yield (f"status={value}", {"status": value})
    for value in TYPES:
        yield (f"type={value}", {"type": value})
    for value in APP_NAMES:
        yield (f"app_name={value}", {"app_name": value})
    for value in SEEN:
        yield (f"seen={value}", {"seen": value})
    for name, window in windows.items():
        yield (f"time={name}", dict(window))


def matrix_cases(windows):
    """
    Every non-empty combination of the filter fields, at one representative
    value each. Combinations including time are run once per window.

    7 fields -> 127 combinations; the 64 containing time run twice, for 191.
    """
    for size in range(1, len(MATRIX_FIELDS) + 1):
        for combo in itertools.combinations(MATRIX_FIELDS, size):
            if "time" in combo:
                for name, window in windows.items():
                    filters = dict(window)
                    for field in combo:
                        if field != "time":
                            filters[field] = REPRESENTATIVE[field]
                    yield ("+".join(combo) + f" [{name}]", filters)
            else:
                filters = {field: REPRESENTATIVE[field] for field in combo}
                yield ("+".join(combo), filters)


def text_cases(windows):
    """
    Text search. Listed separately and marked FTS-only because the server has no
    GSI text path: any query carrying `text` routes to FTS whether or not
    engine=fts is set, so a 'GSI' column here would be FTS measured twice.
    """
    yield ("text", {"text": SAMPLE_TEXT})
    yield ("text+status", {"text": SAMPLE_TEXT, "status": "DELIVERED"})
    yield ("text+channel", {"text": SAMPLE_TEXT, "channel": "email"})
    for name, window in windows.items():
        filters = dict(window)
        filters["text"] = SAMPLE_TEXT
        yield (f"text+time [{name}]", filters)



# The three values measured as GSI-hostile before the partial indexes existed.
# The combination matrix pins one representative value per field, and only
# status=PENDING is among them - so cblite and seen=true are otherwise tested
# only on their own. This phase exercises all three in combination, which is
# where a partial index either holds up or stops helping.
HOSTILE_VALUES = [("status", "PENDING"), ("channel", "cblite"), ("seen", "true")]


def hostile_cases(windows, skip_type=True):
    dims = ["user_id", "app_name", "channel", "status", "seen"]
    if not skip_type:
        dims.append("type")
    for field, value in HOSTILE_VALUES:
        base = {field: value}
        yield (f"{field}={value}", dict(base))
        for wname, window in windows.items():
            d = dict(base); d.update(window)
            yield (f"{field}={value}+time [{wname}]", d)
        for other in dims:
            if other == field:
                continue
            d = dict(base); d[other] = REPRESENTATIVE[other]
            yield (f"{field}={value}+{other}", d)
        deep = dict(base)
        for other in dims:
            if other != field:
                deep[other] = REPRESENTATIVE[other]
        yield (f"{field}={value}+all-dims", deep)
        deep_t = dict(deep); deep_t.update(windows["last30d"])
        yield (f"{field}={value}+all-dims+time [last30d]", deep_t)


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------

def fmt(cell):
    outcome, server_ms, wall_ms, _rows, _total = cell
    if outcome == "ok":
        return f"{server_ms:9.1f}" if server_ms is not None else f"{wall_ms:9.1f}*"
    if outcome == "n/a":
        return "      n/a"   # text phase: there is no GSI text path to measure
    if outcome.startswith("timeout"):
        return "  TIMEOUT" if outcome == "timeout" else "TIMEOUT!"
    return "    ERROR"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default=DEFAULT_BASE, help=f"pipeline base URL (default {DEFAULT_BASE})")
    ap.add_argument("--cb", default=DEFAULT_CB,
                    help=f"cluster URL, used only to cancel queries (default {DEFAULT_CB})")
    ap.add_argument("--gsi-index", default="",
                    help="compare this index against the optimizer's own choice, GSI only, "
                         "via direct N1QL with a USE INDEX hint. Skips the FTS arm and the "
                         "text phase, and never escalates to the slow timeout.")
    ap.add_argument("--skip-type", action="store_true",
                    help="drop the `type` filter everywhere. The dashboard has no type input "
                         "(it is a table column only), so type-bearing shapes are unreachable "
                         "in the UI and inflate the matrix with combinations no user can make.")
    ap.add_argument("--phase", choices=["all", "sweep", "matrix", "text", "hostile"], default="all")
    ap.add_argument("--runs", type=int, default=1, help="attempts per cell, fastest wins (default 1)")
    ap.add_argument("--timeout", type=float, default=FAST_TIMEOUT, help="the limit, seconds (default 5)")
    ap.add_argument("--slow-timeout", type=float, default=SLOW_TIMEOUT,
                    help="ceiling for the both-slow re-run, seconds (default 60)")
    ap.add_argument("--limit", type=int, default=0, help="stop after N combinations (0 = no cap)")
    ap.add_argument("--out", default="", help="CSV path (default bench-filters-<timestamp>.csv here)")
    ap.add_argument("--dry-run", action="store_true", help="list the combinations, run nothing")
    global BASE, CB
    args = ap.parse_args()
    BASE = args.base.rstrip("/")
    CB = args.cb.rstrip("/")

    windows = time_windows()
    if args.skip_type:
        global TYPES, MATRIX_FIELDS
        TYPES = []
        MATRIX_FIELDS = [f for f in MATRIX_FIELDS if f != "type"]
    cases = []
    if args.phase in ("all", "sweep"):
        cases += [("sweep", label, f) for label, f in sweep_cases(windows)]
    if args.phase in ("all", "matrix"):
        cases += [("matrix", label, f) for label, f in matrix_cases(windows)]
    if args.phase in ("all", "hostile"):
        cases += [("hostile", label, f) for label, f in hostile_cases(windows, args.skip_type)]
    if args.phase in ("all", "text"):
        cases += [("text", label, f) for label, f in text_cases(windows)]
    if args.limit:
        cases = cases[: args.limit]

    if args.dry_run:
        for phase, label, filters in cases:
            print(f"{phase:7} {label:52} {urllib.parse.urlencode(filters)}")
        print(f"\n{len(cases)} combinations, {len(cases) * 2} requests")
        return 0

    out_path = args.out or os.path.join(
        os.path.dirname(os.path.abspath(__file__)),
        f"bench-filters-{datetime.now().strftime('%Y%m%d-%H%M%S')}.csv")

    print(f"base    : {BASE}")
    print(f"limit   : {args.timeout}s, re-run at {args.slow_timeout}s only when BOTH exceed it")
    print(f"cancel  : server-side via {CB} (one query in flight at a time)")
    print(f"cases   : {len(cases)} combinations ({len(cases) * 2} requests), best of {args.runs}")
    print(f"windows : last30d {windows['last30d']['from']} .. now | oct2025 {windows['oct2025']['from']} .. {windows['oct2025']['to']}")
    print(f"csv     : {out_path}\n")
    print(f"{'phase':7} {'filters':52} {'GSI ms':>9} {'FTS ms':>9}  winner")
    print("-" * 104)

    rows = []
    both_slow = []
    started = time.perf_counter()

    if args.gsi_index:
        # GSI-only index comparison. Direct N1QL with a USE INDEX hint, because
        # the app takes no index parameter and must not be modified for a test.
        # The query service enforces the limit itself (timeout=<n>s), so a query
        # is cancelled server-side rather than abandoned; no slow-timeout
        # escalation applies here.
        print(f"index   : {args.gsi_index} (direct N1QL, USE INDEX hint; FTS and text phases skipped)\n")
        print(f"{'phase':7} {'filters':52} {'ms':>9} {'plan':>28} {'sort':>5} {'hint':>13}")
        print("-" * 122)
        for phase, label, filters in cases:
            if phase == "text":
                continue    # no GSI text path to pin an index to
            built = build_n1ql(filters, args.gsi_index)
            plan_idx, has_sort, honoured = explain_index(built)
            cell = run_n1ql(built, args.timeout)
            shown = f"{cell[1]:9.1f}" if cell[0] == "ok" else ("  TIMEOUT" if cell[0].startswith("timeout") else "    ERROR")
            print(f"{phase:7} {label:52} {shown} {plan_idx:>28} {'YES' if has_sort else 'no':>5} {honoured:>13}")
            rows.append({
                "phase": phase, "filters": label,
                "query_string": urllib.parse.urlencode(filters),
                "index": args.gsi_index, "outcome": cell[0],
                "server_ms": cell[1], "wall_ms": round(cell[2], 1), "rows": cell[3],
                "plan_indexes": plan_idx, "needs_sort": has_sort, "hint": honoured,
            })
        with open(out_path, "w", newline="") as fh:
            writer = csv.DictWriter(fh, fieldnames=list(rows[0].keys()))
            writer.writeheader(); writer.writerows(rows)
        ok = [r for r in rows if r["outcome"] == "ok"]
        to = [r for r in rows if str(r["outcome"]).startswith("timeout")]
        print("-" * 122)
        print(f"\n{len(rows)} cells, {time.perf_counter() - started:.0f}s elapsed")
        print(f"  completed under {args.timeout:.0f}s : {len(ok)}")
        print(f"  timed out            : {len(to)}")
        print(f"  plans needing a sort : {sum(1 for r in rows if r['needs_sort'])}")
        print(f"  hint not followed    : {sum(1 for r in rows if r['hint'] == 'NOT-FOLLOWED')}")
        if ok:
            best = sorted(ok, key=lambda r: r["server_ms"])[:5]
            print("\n  fastest cells:")
            for r in best:
                print(f"    {r['filters']:52} {r['server_ms']:9.1f} ms")
        print(f"\nCSV: {out_path}")
        return 0

    for phase, label, filters in cases:
        if phase == "text":
            # FTS-only by construction; no GSI arm to measure.
            fts = best_of(filters, "fts", args.timeout, args.runs)
            gsi = ("n/a", None, 0.0, 0, None)
            winner = "FTS only"
        else:
            gsi = best_of(filters, "gsi", args.timeout, args.runs)
            fts = best_of(filters, "fts", args.timeout, args.runs)

            if gsi[0].startswith("timeout") and fts[0].startswith("timeout"):
                # Both blew the limit: re-run once each with the higher ceiling
                # so the slower engine is still identified.
                gsi = query(filters, "gsi", args.slow_timeout)
                fts = query(filters, "fts", args.slow_timeout)
                both_slow.append((label, gsi, fts))

            if gsi[0] == "ok" and fts[0] == "ok":
                g, f = gsi[1] or gsi[2], fts[1] or fts[2]
                winner = f"GSI {f / g:.0f}x" if g < f else f"FTS {g / f:.0f}x"
            elif gsi[0] == "ok":
                winner = "GSI"
            elif fts[0] == "ok":
                winner = "FTS"
            else:
                winner = "neither"

        note = diagnose(gsi, fts) if phase != "text" else "no GSI text path"
        print(f"{phase:7} {label:52} {fmt(gsi)} {fmt(fts)}  {winner}"
              + (f"   [{note}]" if note and "slow" in note else ""))

        rows.append({
            "phase": phase, "filters": label,
            "query_string": urllib.parse.urlencode(filters),
            "gsi_outcome": gsi[0], "gsi_server_ms": gsi[1], "gsi_wall_ms": round(gsi[2], 1),
            "fts_outcome": fts[0], "fts_server_ms": fts[1], "fts_wall_ms": round(fts[2], 1),
            "fts_total_hits": fts[4], "rows_gsi": gsi[3], "rows_fts": fts[3],
            "winner": winner, "diagnosis": note,
        })

    with open(out_path, "w", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    elapsed = time.perf_counter() - started
    measured = [r for r in rows if r["phase"] != "text"]
    gsi_wins = sum(1 for r in measured if r["winner"].startswith("GSI"))
    fts_wins = sum(1 for r in measured if r["winner"].startswith("FTS"))
    gsi_to = sum(1 for r in measured if str(r["gsi_outcome"]).startswith("timeout"))
    fts_to = sum(1 for r in measured if str(r["fts_outcome"]).startswith("timeout"))

    print("-" * 104)
    print(f"\n{len(measured)} compared, {elapsed:.0f}s elapsed")
    print(f"  GSI faster : {gsi_wins}")
    print(f"  FTS faster : {fts_wins}")
    print(f"  over {args.timeout:.0f}s : GSI {gsi_to}, FTS {fts_to}")
    if both_slow:
        print(f"\n  both over the limit ({len(both_slow)}), re-run at {args.slow_timeout:.0f}s:")
        for label, gsi, fts in both_slow:
            print(f"    {label:52} GSI {fmt(gsi)}  FTS {fmt(fts)}")
    print(f"\nCSV: {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
