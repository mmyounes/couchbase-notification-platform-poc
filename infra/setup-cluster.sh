#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# Notification Platform PoC - Couchbase cluster configuration
#
# Idempotent: safe to re-run. Creates nothing that already exists and never
# deletes or overwrites data.
#
# Usage:
#   CB_HOST=192.168.0.10 CB_PASS=password ./setup-cluster.sh
#   ./setup-cluster.sh --dry-run        # print what would happen, change nothing
#
# Run this from a host with network access to the cluster (e.g. the app EC2
# instance in the same VPC).
# ---------------------------------------------------------------------------
set -euo pipefail

CB_HOST="${CB_HOST:?set CB_HOST to a Couchbase node address}"
CB_PORT="${CB_PORT:-8091}"
CB_USER="${CB_USER:-Administrator}"
CB_PASS="${CB_PASS:-password}"

BUCKET="${BUCKET:-ncgr}"
SCOPE="${SCOPE:-platform}"

# PROFILE picks a default sizing. Every value below can still be overridden
# individually; the profile only changes what they default to.
#
#   full   (default) the benchmarked 7-node cluster: 3 data, 2 index+query,
#          2 search. Sized for 500M documents.
#   small  3 nodes, all services co-located, ~16 GiB each. Good for a few
#          million documents - every feature works, the numbers do not.
#   single 1 node, all services, ~8 GiB. REPLICAS must be 0: Couchbase cannot
#          place a replica on the only node, and the bucket create fails
#          outright rather than degrading.
#
# Quotas are MiB PER NODE and are read back and asserted (see verify_quotas).
PROFILE="${PROFILE:-full}"

case "$PROFILE" in
  full)
    DATA_QUOTA_MB="${DATA_QUOTA_MB:-51200}"    # 50 GiB of 64 GiB data nodes
    INDEX_QUOTA_MB="${INDEX_QUOTA_MB:-98304}"  # 96 GiB of 128 GiB index+query
    FTS_QUOTA_MB="${FTS_QUOTA_MB:-102400}"     # 100 GiB of 128 GiB search
    BUCKET_RAM_MB="${BUCKET_RAM_MB:-51200}"    # per node; 3 data nodes => 150 GiB
    REPLICAS="${REPLICAS:-1}"
    ;;
  small)
    DATA_QUOTA_MB="${DATA_QUOTA_MB:-6144}"
    INDEX_QUOTA_MB="${INDEX_QUOTA_MB:-4096}"
    FTS_QUOTA_MB="${FTS_QUOTA_MB:-2048}"
    BUCKET_RAM_MB="${BUCKET_RAM_MB:-6144}"
    REPLICAS="${REPLICAS:-1}"
    ;;
  single)
    DATA_QUOTA_MB="${DATA_QUOTA_MB:-3072}"
    INDEX_QUOTA_MB="${INDEX_QUOTA_MB:-2048}"
    FTS_QUOTA_MB="${FTS_QUOTA_MB:-1024}"
    BUCKET_RAM_MB="${BUCKET_RAM_MB:-3072}"
    REPLICAS="${REPLICAS:-0}"                  # one node cannot host a replica
    ;;
  *)
    echo "unknown PROFILE '$PROFILE' (expected: full | small | single)" >&2
    exit 2
    ;;
esac

NUM_VBUCKETS="${NUM_VBUCKETS:-1024}"

# Counters collection: TTL backstop. Longest window we use is the per-receiver
# hourly cap, so 2h. Per-document TTLs are shorter and win over this.
COUNTERS_MAX_TTL=7200

DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

BASE="http://${CB_HOST}:${CB_PORT}"
AUTH=(-u "${CB_USER}:${CB_PASS}")
# N1QL and FTS endpoints are resolved during preflight: if CB_HOST does not
# itself run the query/search service (e.g. it is a data-only node), we fall
# back to the cluster manager's UI proxy paths on 8091, which reach whichever
# node does. Set N1QL_URL / FTS_URL to override.
N1QL=""
FTS=""

# --- helpers ---------------------------------------------------------------

c_red()  { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
c_yel()  { printf '\033[33m%s\033[0m\n' "$*"; }
step()   { printf '\n\033[1m== %s\033[0m\n' "$*"; }
skip()   { printf '   - %s\n' "$*"; }
did()    { c_grn "   + $*"; }

# Run a mutating call, or describe it under --dry-run.
mutate() {
  local desc="$1"; shift
  if (( DRY_RUN )); then
    c_yel "   ~ would: ${desc}"
    return 0
  fi
  local out code
  out="$(curl -sS -m 120 -w '\n%{http_code}' "${AUTH[@]}" "$@" 2>&1)" || {
    c_red "   ! FAILED: ${desc}"; printf '%s\n' "$out"; return 1; }
  code="$(tail -n1 <<<"$out")"
  if [[ "$code" =~ ^2 ]]; then
    did "$desc"
  else
    c_red "   ! FAILED (HTTP ${code}): ${desc}"
    sed '$d' <<<"$out"
    return 1
  fi
}

get() { curl -sS -m 30 "${AUTH[@]}" "$@"; }

# True only on HTTP 2xx. `curl -sS` exits 0 on 404, so existence checks MUST
# test the status code rather than the exit code.
exists() {
  local code
  code="$(curl -sS -m 30 -o /dev/null -w '%{http_code}' "${AUTH[@]}" "$1" 2>/dev/null || echo 000)"
  [[ "$code" =~ ^2 ]]
}

# Execute a N1QL statement. Tolerates "already exists" so re-runs are clean.
n1ql() {
  local stmt="$1" desc="$2"
  if (( DRY_RUN )); then c_yel "   ~ would: ${desc}"; return 0; fi
  local out
  out="$(curl -sS -m 300 "${AUTH[@]}" "$N1QL" --data-urlencode "statement=${stmt}" 2>&1)"
  if grep -q '"status": *"success"' <<<"$out"; then
    did "$desc"
  elif grep -qiE 'already exist|duplicate index' <<<"$out"; then
    skip "${desc} (already exists)"
  else
    c_red "   ! FAILED: ${desc}"; printf '%s\n' "$out"; return 1
  fi
}

# --- 0. preflight ----------------------------------------------------------

step "Preflight"
if ! pools="$(get "${BASE}/pools" 2>&1)"; then
  c_red "Cannot reach ${BASE}"
  c_red "Check: node is up, you are inside the VPC, security group allows 8091."
  exit 1
fi
VERSION="$(sed -n 's/.*"implementationVersion":"\([^"]*\)".*/\1/p' <<<"$pools")"
c_grn "   Connected. Server version: ${VERSION:-unknown}"

# numVBuckets is only settable on 7.6+. On older versions 1024 is the fixed
# default, so omitting the parameter yields the same result.
SUPPORTS_VBUCKET_PARAM=0
case "$VERSION" in
  7.6*|7.[7-9]*|8.*|9.*) SUPPORTS_VBUCKET_PARAM=1 ;;
esac

# Resolve service endpoints. Prefer the direct port; fall back to the UI proxy.
reachable() { curl -sS -m 8 -o /dev/null "${AUTH[@]}" "$1" >/dev/null 2>&1; }

if [[ -n "${N1QL_URL:-}" ]]; then
  N1QL="$N1QL_URL"
elif reachable "http://${CB_HOST}:8093/admin/ping"; then
  N1QL="http://${CB_HOST}:8093/query/service"
else
  N1QL="${BASE}/_p/query/query/service"
  c_yel "   query service not on ${CB_HOST}:8093 - using UI proxy"
fi

if [[ -n "${FTS_URL:-}" ]]; then
  FTS="$FTS_URL"
elif reachable "http://${CB_HOST}:8094/api/ping"; then
  FTS="http://${CB_HOST}:8094"
else
  FTS="${BASE}/_p/fts"
  c_yel "   search service not on ${CB_HOST}:8094 - using UI proxy"
fi
c_grn "   n1ql -> ${N1QL}"
c_grn "   fts  -> ${FTS}"

step "Cluster topology"
get "${BASE}/pools/default" \
  | tr ',' '\n' | grep -E '"(hostname|services|status)"' | head -40 \
  || true

# --- 1. service memory quotas ---------------------------------------------

step "Service memory quotas (per node)"
mutate "data=${DATA_QUOTA_MB}MiB index=${INDEX_QUOTA_MB}MiB fts=${FTS_QUOTA_MB}MiB" \
  -X POST "${BASE}/pools/default" \
  -d "memoryQuota=${DATA_QUOTA_MB}" \
  -d "indexMemoryQuota=${INDEX_QUOTA_MB}" \
  -d "ftsMemoryQuota=${FTS_QUOTA_MB}"

# --- 2. bucket -------------------------------------------------------------
# Magma and vBucket count are IMMUTABLE after creation. If the bucket exists
# with the wrong storage backend the only fix is delete + reload, so we assert
# loudly rather than quietly continuing.

step "Bucket '${BUCKET}'"
if exists "${BASE}/pools/default/buckets/${BUCKET}"; then
  existing="$(get "${BASE}/pools/default/buckets/${BUCKET}")"
  backend="$(sed -n 's/.*"storageBackend":"\([^"]*\)".*/\1/p' <<<"$existing")"
  skip "bucket exists (storageBackend=${backend:-?})"
  if [[ "$backend" != "magma" ]]; then
    c_red "   ! Bucket exists but storageBackend is '${backend}', not magma."
    c_red "   ! This CANNOT be changed in place. Delete the bucket and re-run"
    c_red "   ! (destroys all data in it) before seeding 500M documents."
    exit 1
  fi
else
  args=(
    -d "name=${BUCKET}"
    -d "bucketType=couchbase"
    -d "ramQuota=${BUCKET_RAM_MB}"
    -d "replicaNumber=${REPLICAS}"
    -d "storageBackend=magma"
    -d "evictionPolicy=fullEviction"   # required by Magma; valueOnly unsupported
    -d "compressionMode=active"
    -d "conflictResolutionType=seqno"
    -d "durabilityMinLevel=none"
    -d "flushEnabled=1"               # PoC convenience: allows a clean reset
  )
  (( SUPPORTS_VBUCKET_PARAM )) && args+=(-d "numVBuckets=${NUM_VBUCKETS}")
  mutate "create bucket ${BUCKET} (magma, ${REPLICAS} replica, ${BUCKET_RAM_MB}MiB/node)" \
    -X POST "${BASE}/pools/default/buckets" "${args[@]}"

  if (( ! DRY_RUN )); then
    printf '   waiting for bucket to become healthy'
    for _ in $(seq 1 60); do
      if get "${BASE}/pools/default/buckets/${BUCKET}" 2>/dev/null | grep -q '"status":"healthy"'; then
        printf ' ok\n'; break
      fi
      printf '.'; sleep 2
    done
    echo
  fi
fi

# --- 3. scope and collections ---------------------------------------------

step "Scope '${SCOPE}' and collections"
if get "${BASE}/pools/default/buckets/${BUCKET}/scopes" 2>/dev/null \
     | grep -q "\"name\":\"${SCOPE}\""; then
  skip "scope ${SCOPE}"
else
  mutate "create scope ${SCOPE}" \
    -X POST "${BASE}/pools/default/buckets/${BUCKET}/scopes" -d "name=${SCOPE}"
fi

scopes_json="$(get "${BASE}/pools/default/buckets/${BUCKET}/scopes" 2>/dev/null || echo '')"
create_collection() {
  local name="$1" ttl="${2:-0}"
  if grep -q "\"name\":\"${name}\"" <<<"$scopes_json"; then
    skip "collection ${name}"; return 0
  fi
  local args=(-d "name=${name}")
  [[ "$ttl" != "0" ]] && args+=(-d "maxTTL=${ttl}")
  mutate "create collection ${name}$([[ $ttl != 0 ]] && echo " (maxTTL=${ttl}s)")" \
    -X POST "${BASE}/pools/default/buckets/${BUCKET}/scopes/${SCOPE}/collections" "${args[@]}"
}

# Configuration Data
create_collection tenants
create_collection event_defs
create_collection templates
create_collection policies
# Policy State - TTL backstop so stale counters can never accumulate
create_collection counters "${COUNTERS_MAX_TTL}"
# Notification Data
create_collection events
create_collection notifications
# Device Management (stub; populated by the deferred Sync Gateway work)
create_collection devices

# Device Registry aggregate, maintained by the `aggregate_user_devices` Eventing
# function (infra/eventing/). One document per user, devices nested by app.
create_collection users

# Eventing's own checkpoint/state store. It must NOT be a collection any handler
# reads or writes, and it must exist before a function referencing it deploys.
create_collection eventing_metadata

(( DRY_RUN )) || sleep 5   # let collection manifest propagate before DDL

# --- 4. GSI indexes -------------------------------------------------------
# Large indexes are DEFERRED. Build them AFTER the 500M seed completes:
#   BUILD INDEX ON `<bucket>`.`<scope>`.`notifications`
#     (idx_multi, idx_user_multi, idx_retry,
#      idx_pending_feed, idx_cblite_feed, idx_seen_feed);
# Building during the load is dramatically slower. Measured: ~50 min per
# composite index over 514M items on 2x (16 vCPU / 123.5 GB) index nodes.
#
# Two composites, deliberately. Both lead with tenant_id, then diverge:
#
#   idx_multi       (tenant_id, sort_key DESC, status, channel, app_name, type, seen)
#   idx_user_multi  (tenant_id, user_id, sort_key DESC, status, channel, app_name, type, seen)
#
# The split exists because a range predicate on a composite key demotes every
# key after it to a post-filter. idx_multi answers the time-ordered feed and any
# keyword filter on it; but with sort_key ranged, a trailing user_id is only a
# post-filter, so a single-user query scanned the whole tenant - measured at
# 49.3 SECONDS. idx_user_multi puts user_id BEFORE the range, making it a seek:
# the same query is 1.6 ms. Do not merge these two.
#
# Superseded by the above (do not re-add):
#   idx_feed (tenant_id, sort_key DESC)            - exact key prefix of idx_multi.
#   idx_user (tenant_id, user_id, sort_key DESC)   - exact key prefix of idx_user_multi.
# Both were redundant and cost ~30 GB and ~33 GB of disk per index node. Dropped
# after verifying 19 query shapes: every one kept its index and its index_order,
# the only change being the unfiltered feed at 1.6 -> 1.7 ms.

step "GSI indexes on notifications (deferred build)"
K="\`${BUCKET}\`.\`${SCOPE}\`"

n1ql "CREATE INDEX \`idx_multi\` ON ${K}.\`notifications\`(\`tenant_id\`, \`sort_key\` DESC,
      \`status\`, \`channel\`, \`app_name\`, \`type\`, \`seen\`, \`user_id\`)
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_multi (tenant_id, sort_key DESC, + keyword filters)"

n1ql "CREATE INDEX \`idx_user_multi\` ON ${K}.\`notifications\`(\`tenant_id\`, \`user_id\`,
      \`sort_key\` DESC, \`status\`, \`channel\`, \`app_name\`, \`type\`, \`seen\`)
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_user_multi (tenant_id, user_id, sort_key DESC, + keyword filters)"

# Three partial indexes, one per value that idx_multi cannot serve.
#
# idx_multi leads with (tenant_id, sort_key DESC), so every other key after the
# range is a post-filter: the scan walks the feed in time order and tests each
# entry. That is fine when matches are near the top, and catastrophic when they
# are not. Measured on 522M documents BEFORE these indexes existed:
#
#   status=PENDING    82,366 ms   pending work is old; ~20M newer entries first
#   channel=cblite     timeout    5 documents in the whole collection
#   seen=true          timeout    only seeded data carries it
#
# Each index below indexes only its own value, so the scan starts inside the
# matching set and stops at LIMIT. After adding them: 6.2 ms, 3.4 ms, 7.9 ms.
#
# They are cheap because they are partial - idx_pending_feed holds 25M entries
# against idx_multi's 522M, about 6 GB against 86 GB.
#
# NOTE: db.go writes these three values as LITERALS rather than bound
# parameters. A partial index can only be chosen when the planner can prove its
# condition holds, and the pipeline prepares its statements with the parameters
# still unbound. With a parameter the planner silently falls back to idx_multi
# and the timeouts above return, with no error to explain why.
n1ql "CREATE INDEX \`idx_pending_feed\` ON ${K}.\`notifications\`(\`tenant_id\`,
      \`sort_key\` DESC, \`channel\`, \`app_name\`, \`seen\`, \`user_id\`)
      WHERE \`status\` = \"PENDING\"
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_pending_feed (partial, WHERE status='PENDING')"

n1ql "CREATE INDEX \`idx_cblite_feed\` ON ${K}.\`notifications\`(\`tenant_id\`,
      \`sort_key\` DESC, \`status\`, \`app_name\`, \`seen\`, \`user_id\`)
      WHERE \`channel\` = \"cblite\"
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_cblite_feed (partial, WHERE channel='cblite')"

n1ql "CREATE INDEX \`idx_seen_feed\` ON ${K}.\`notifications\`(\`tenant_id\`,
      \`sort_key\` DESC, \`status\`, \`channel\`, \`app_name\`, \`user_id\`)
      WHERE \`seen\` = true
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_seen_feed (partial, WHERE seen=true)"

# Partial index: only in-flight and backing-off work. Stays in the thousands
# even when the collection holds 500M documents.
n1ql "CREATE INDEX \`idx_retry\` ON ${K}.\`notifications\`(\`delivery\`.\`next_attempt_at\`)
      WHERE \`status\` = 'PENDING'
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_retry (partial, WHERE status='PENDING')"

step "GSI index on events (deferred build)"
# Partial: only events where something was suppressed. Small.
n1ql "CREATE INDEX \`idx_evt_suppressed\` ON ${K}.\`events\`(\`tenant_id\`, \`submitted_at\` DESC)
      WHERE \`suppressed\` IS NOT MISSING
      WITH {\"defer_build\": true, \"num_replica\": ${REPLICAS}}" \
     "idx_evt_suppressed (partial)"

step "Primary indexes on config collections (built immediately - tiny)"
for c in tenants event_defs templates policies devices; do
  n1ql "CREATE PRIMARY INDEX ON ${K}.\`${c}\` WITH {\"num_replica\": ${REPLICAS}}" \
       "primary index on ${c}"
done
# Deliberately NO primary index on notifications, events, or counters.
# At 500M documents a primary index is a footgun and every query we run is covered.

# --- 5. FTS index ---------------------------------------------------------

step "FTS index 'fts_notifications'"
FTS_DEF="$(dirname "$0")/fts-notifications.json"
if [[ ! -f "$FTS_DEF" ]]; then
  c_red "   ! ${FTS_DEF} not found - skipping FTS index"
elif exists "${FTS}/api/index/fts_notifications"; then
  skip "fts_notifications (already exists)"
else
  mutate "create FTS index fts_notifications" \
    -X PUT "${FTS}/api/index/fts_notifications" \
    -H 'Content-Type: application/json' \
    --data-binary "@${FTS_DEF}"
fi

# --- 6. verification ------------------------------------------------------

step "Verification"
if (( DRY_RUN )); then c_yel "   (skipped under --dry-run)"; exit 0; fi

get "${BASE}/pools/default/buckets/${BUCKET}" | python3 -c '
import json,sys
b=json.load(sys.stdin)
rows=[("storageBackend",b.get("storageBackend")),("evictionPolicy",b.get("evictionPolicy")),
      ("compressionMode",b.get("compressionMode")),("conflictResolution",b.get("conflictResolutionType")),
      ("durabilityMinLevel",b.get("durabilityMinLevel")),("replicaNumber",b.get("replicaNumber")),
      ("numVBuckets",b.get("numVBuckets")),
      ("ramQuota per node",   "%d MiB" % (b["quota"]["rawRAM"]//1048576)),
      ("ramQuota cluster",    "%d MiB" % (b["quota"]["ram"]//1048576)),
      ("data nodes serving",  len(b.get("nodes",[])))]
expect={"storageBackend":"magma","evictionPolicy":"fullEviction","compressionMode":"active"}
bad=0
for k,v in rows:
    flag=""
    if k in expect and str(v)!=expect[k]: flag="  <== EXPECTED %s" % expect[k]; bad=1
    print("   %-20s %s%s" % (k,v,flag))
sys.exit(1 if bad else 0)
' || c_red "   ! bucket settings do not match the design - see above"

echo
echo "   Index states (all large indexes should read 'deferred'):"
curl -sS -m 60 "${AUTH[@]}" "$N1QL" --data-urlencode \
  "statement=SELECT name, state, keyspace_id FROM system:indexes WHERE bucket_id='${BUCKET}' ORDER BY keyspace_id, name" \
  | python3 -c '
import json,sys
for r in json.load(sys.stdin).get("results",[]):
    print("   %-22s %-16s %s" % (r.get("name"), r.get("keyspace_id"), r.get("state")))
' || true

cat <<EOF

---------------------------------------------------------------------------
NEXT STEPS (in order)  [profile: ${PROFILE}]

  1. Verify above: storageBackend=magma, evictionPolicy=fullEviction,
     numVBuckets=${NUM_VBUCKETS}, replicaNumber=${REPLICAS}.
     If storageBackend is not magma, STOP - fix before seeding.

  2. Seed the 500M notifications (hours). Indexes stay deferred throughout.

  3. Build the GSI indexes only after seeding completes:
       BUILD INDEX ON `<bucket>`.`<scope>`.`notifications`
         (idx_multi, idx_user_multi, idx_retry,
          idx_pending_feed, idx_cblite_feed, idx_seen_feed);
       BUILD INDEX ON `<bucket>`.`<scope>`.`events`(idx_evt_suppressed);

  4. FTS will index continuously as documents land. Building it over 500M
     documents is a multi-hour job - expect to leave it overnight, and watch
     search-node RAM: this is the tightest-fitting component in the cluster.
---------------------------------------------------------------------------
EOF
