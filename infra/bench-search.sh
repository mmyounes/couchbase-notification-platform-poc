#!/usr/bin/env bash
# Search latency through the real API, after routing keyword filters to the
# composite GSI and leaving FTS for message text only.
set -uo pipefail
B=${B:-http://localhost:3000/api}
RUNS=${RUNS:-3}

t () {
  local label="$1" qs="$2" best=99999 ms
  for _ in $(seq 1 "$RUNS"); do
    ms=$(curl -sS -o /tmp/r.json -m 180 -w '%{time_total}' "$B/notifications$qs" \
         | awk '{printf "%.0f", $1*1000}')
    if [ "$ms" -lt "$best" ] 2>/dev/null; then best=$ms; fi
  done
  LABEL="$label" MS="$best" python3 - <<'PY'
import json, os
d = json.load(open('/tmp/r.json'))
if 'error' in d:
    print('  %-44s %7s ms  ERROR %s' % (os.environ['LABEL'], os.environ['MS'], str(d['error'])[:50]))
else:
    rows = len(d.get('rows') or [])
    verdict = 'ok' if rows and not d.get('incomplete') else 'EMPTY'
    print('  %-44s %7s ms  %-6s path=%-4s rows=%-4d total=%s'
          % (os.environ['LABEL'], os.environ['MS'], verdict, d.get('path'), rows, d.get('total')))
PY
}

echo "=== keyword filters (now GSI via idx_multi) ==="
t "latest feed"                      ""
t "status=FAILED"                    "?status=FAILED"
t "channel=email + status=DELIVERED" "?channel=email&status=DELIVERED"
t "app_name=portal"                  "?app_name=portal"
t "seen=false + channel=push"        "?seen=false&channel=push"
t "type=otp_login"                   "?type=otp_login"
t "user_id"                          "?user_id=usr_000042"
t "explicit date range"              "?from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z"
echo "=== message text (still FTS, unavoidably) ==="
t "text only"                        "?text=temporary%20password"
t "text + status"                    "?text=invoice&status=DELIVERED"
t "text + 1-day range"               "?text=invoice&from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z"
