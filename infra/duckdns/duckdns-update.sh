#!/usr/bin/env bash
# Update this host's DuckDNS A record to whatever public address the request
# arrives from. Called at boot and every 10 min by duckdns@<domain>.timer.
#
# The token is piped to curl via -K so it never appears in argv (visible to any
# local user via ps) nor in the journal. Only OK/KO is logged.
#
# Leaving ip= empty is deliberate: DuckDNS then uses the source address of the
# request, so a restarted instance with a fresh public IP repairs its own record
# with no input. That is the whole point of this unit - do not pass an explicit
# ip= here.
set -uo pipefail

DOMAIN="${1:?usage: duckdns-update.sh <subdomain>}"
TOKEN_FILE=/etc/duckdns/token

[ -r "$TOKEN_FILE" ] || { echo "duckdns: cannot read $TOKEN_FILE"; exit 1; }
TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"

resp="$(printf 'url = "https://www.duckdns.org/update?domains=%s&token=%s&ip="\n' \
  "$DOMAIN" "$TOKEN" | curl -sS -m 30 -K - 2>&1)"

case "$resp" in
  OK*) echo "duckdns: $DOMAIN updated OK"; exit 0 ;;
  *)   echo "duckdns: $DOMAIN update FAILED (response: ${resp:-empty})"; exit 1 ;;
esac
