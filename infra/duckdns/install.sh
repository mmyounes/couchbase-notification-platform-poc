#!/usr/bin/env bash
# Install the DuckDNS self-updater on this host for one subdomain.
#
#   sudo DUCKDNS_TOKEN=<token> bash install.sh <subdomain>
#
# Pass the token in the environment rather than as an argument so it does not
# land in this host's shell history or in ps output. Idempotent: re-running
# refreshes the files and restarts the timer.
set -euo pipefail

DOMAIN="${1:?usage: DUCKDNS_TOKEN=<token> install.sh <subdomain>   (e.g. ncgr-app)}"
TOKEN="${DUCKDNS_TOKEN:-}"
[ -n "$TOKEN" ] || { echo "set DUCKDNS_TOKEN in the environment" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 2; }

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

install -d -m 700 -o root -g root /etc/duckdns
printf '%s\n' "$TOKEN" > /etc/duckdns/token
chmod 600 /etc/duckdns/token; chown root:root /etc/duckdns/token

install -m 755 -o root -g root "$SRC/duckdns-update.sh" /usr/local/sbin/duckdns-update.sh
install -m 644 -o root -g root "$SRC/duckdns@.service"  /etc/systemd/system/duckdns@.service
install -m 644 -o root -g root "$SRC/duckdns@.timer"    /etc/systemd/system/duckdns@.timer

systemctl daemon-reload
systemctl enable --now "duckdns@${DOMAIN}.timer"
systemctl restart "duckdns@${DOMAIN}.service"

echo "--- result ---"
journalctl -u "duckdns@${DOMAIN}.service" -n 3 --no-pager | tail -2
systemctl list-timers "duckdns@${DOMAIN}.timer" --no-pager | sed -n 2p
