#!/usr/bin/env bash
# Poll the FTS rebuild from the operator's machine via the cluster manager REST
# API. Deliberately holds NO long-lived SSH session: a ~1h ssh invocation gets
# reset by an idle timeout and takes the remote watcher down with it. Only the
# final benchmark shells out, in a short-lived connection.
set -uo pipefail
CB=${CB:-http://localhost:8091}
USERPASS=${USERPASS:-Administrator:password}
TARGET=${TARGET:-500000000}
IDX=${IDX:-fts_notifications}
KEY=${KEY:-${SSH_KEY:-~/.ssh/my-poc-key.pem}}
APP=${APP:-ubuntu@localhost}

count() {
  curl -sS -m 30 -u "$USERPASS" "$CB/_p/fts/api/index/$IDX/count" 2>/dev/null \
    | python3 -c "import json,sys;print(json.load(sys.stdin).get('count',0))" 2>/dev/null || echo -1
}

prev=-1; stalls=0; t0=$(date +%s); start_n=$(count)
echo "$(date -u +%H:%M:%S) starting at $start_n / $TARGET"
while true; do
  n=$(count)
  if [ "$n" -lt 0 ]; then echo "$(date -u +%H:%M:%S) cluster unreachable, retrying"; sleep 60; continue; fi
  now=$(date +%s); el=$(( now - t0 ))
  rate=0; [ "$el" -gt 0 ] && rate=$(( (n - start_n) / el ))
  pct=$(python3 -c "print('%.2f' % (100*$n/$TARGET))")
  eta='-'
  [ "$rate" -gt 0 ] && [ "$n" -lt "$TARGET" ] && eta=$(python3 -c "
s=($TARGET-$n)//$rate; print('%dh%02dm' % (s//3600,(s%3600)//60))")
  echo "$(date -u +%H:%M:%S) indexed=$n (${pct}%) rate=${rate}/s eta=$eta"

  [ "$n" -ge "$TARGET" ] && { echo "REBUILD COMPLETE"; break; }
  if [ "$n" -eq "$prev" ]; then
    stalls=$((stalls+1))
    [ "$stalls" -ge 8 ] && { echo "STALLED at $n"; break; }
  else stalls=0; fi
  prev=$n
  sleep 120
done

echo
echo "======== BENCHMARK: 32 partitions, sort_key removed ========"
ssh -i "$KEY" -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=30 "$APP" '/tmp/bench.sh' 2>&1
echo
echo "======== index size after trimming (was 269.5 GB at 8 partitions) ========"
curl -sS -m 60 -u "$USERPASS" "$CB/_p/fts/api/nsstats" 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
disk=sum(v for k,v in d.items() if 'fts_notifications' in k and k.endswith('num_bytes_used_disk'))
print('  FTS index on disk: %.1f GB' % (disk/1024**3))
" 2>/dev/null || echo "  (stats unavailable)"
