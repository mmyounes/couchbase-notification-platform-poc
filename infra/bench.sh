#!/usr/bin/env bash
# Query latency benchmark against the pipeline API.
# Verifies each response actually contains rows - a fast empty page is a
# failure mode, not a fast query.
set -uo pipefail
B=${B:-http://localhost:8080}
RUNS=${RUNS:-3}

T() {
  local label="$1" url="$2"
  local times=()
  for _ in $(seq 1 "$RUNS"); do
    t=$(curl -sS -o /tmp/last.json -m 120 -w '%{time_total}' "$url" 2>/dev/null || echo 0)
    times+=("$(awk -v x="$t" 'BEGIN{printf "%.0f", x*1000}')")
  done
  local med
  med=$(printf '%s\n' "${times[@]}" | sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}')
  LABEL="$label" MED="$med" python3 - <<'PY'
import json, os
label, med = os.environ['LABEL'], os.environ['MED']
try:
    d = json.load(open('/tmp/last.json'))
except Exception as e:
    print('  %-38s %7sms  UNPARSEABLE (%s)' % (label, med, e)); raise SystemExit
if 'error' in d:
    print('  %-38s %7sms  ERROR %s' % (label, med, str(d['error'])[:58])); raise SystemExit
rows, total, inc = len(d.get('rows') or []), d.get('total'), d.get('incomplete')
verdict = 'ok' if rows and not inc else ('EMPTY' if not rows else 'PARTIAL')
win = d.get('appliedWindowFrom')
print('  %-38s %7sms  %-7s rows=%-4d total=%-11s%s'
      % (label, med, verdict, rows, total, '  window=' + win[:10] if win else ''))
PY
}

echo "=== GSI path (idx_multi / idx_user_multi) ==="
T "latest feed, page 1"            "$B/notifications"
T "user lookup"                    "$B/notifications?user_id=usr_000042"
echo "=== FTS path ==="
T "text (default 7d window)"       "$B/notifications?text=temporary%20password"
T "hybrid text+channel"            "$B/notifications?text=verification%20code&channel=sms"
T "exact only: status=FAILED"      "$B/notifications?status=FAILED"
T "text + explicit 1-day window"   "$B/notifications?text=invoice&from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z"
T "text over FULL 12-month history" "$B/notifications?text=temporary%20password&from=2025-08-01T00:00:00Z&to=2026-08-26T00:00:00Z"
