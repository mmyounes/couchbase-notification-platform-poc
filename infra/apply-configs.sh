#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# Apply the FTS index, Sync Gateway database and Eventing function definitions,
# substituting ${CB_BUCKET}, ${CB_SCOPE} and ${TENANT_ID} from the environment.
#
# The three JSON files are templates on purpose: they are posted verbatim to
# Couchbase APIs, so they cannot read configuration the way the Go services do.
#
#   set -a && . infra/demo.env && set +a
#   ./infra/apply-configs.sh fts        # full-text index
#   ./infra/apply-configs.sh eventing   # device-registry function
#   ./infra/apply-configs.sh sg         # print the Sync Gateway config
#
# Needs envsubst (GNU gettext): brew install gettext | apt install gettext-base
# ---------------------------------------------------------------------------
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CB_BUCKET="${CB_BUCKET:-ncgr}"
CB_SCOPE="${CB_SCOPE:-platform}"
TENANT_ID="${TENANT_ID:-ncgr}"
KC_REALM="${KC_REALM:-myrealm}"
CB_USER="${CB_USER:-Administrator}"
CB_PASS="${CB_PASS:-password}"
CB_HOST="${CB_HOST:-${CB_HOST_PRIVATE:-127.0.0.1}}"
export CB_BUCKET CB_SCOPE TENANT_ID KC_REALM

command -v envsubst >/dev/null || {
  echo "envsubst not found - install GNU gettext" >&2; exit 2; }

render () {
  # Only these four names are substituted; anything else that looks like a shell
  # variable in these files is left alone.
  envsubst '${CB_BUCKET} ${CB_SCOPE} ${TENANT_ID} ${KC_REALM}' < "$1"
}

case "${1:-}" in
  fts)
    echo "FTS index fts_notifications -> ${CB_BUCKET}.${CB_SCOPE}.notifications"
    render "$HERE/fts-notifications.json" \
      | curl -sS -m 120 -u "$CB_USER:$CB_PASS" -X PUT \
          "http://$CB_HOST:8094/api/index/fts_notifications" \
          -H 'Content-Type: application/json' --data-binary @- \
      | head -c 400; echo
    ;;
  eventing)
    echo "Eventing aggregate_user_devices -> ${CB_BUCKET}.${CB_SCOPE}"
    render "$HERE/eventing/aggregate_user_devices.json" \
      | curl -sS -m 120 -u "$CB_USER:$CB_PASS" -X POST \
          "http://$CB_HOST:8096/api/v1/functions/aggregate_user_devices" \
          -H 'Content-Type: application/json' --data-binary @- \
      | head -c 400; echo
    ;;
  sg)
    # Printed, not posted: the admin port is loopback-only on the Sync Gateway
    # host, so this has to be applied from there. See infra/sg/README.md.
    render "$HERE/sg/db-config.json"
    ;;
  *)
    echo "usage: $0 {fts|eventing|sg}" >&2; exit 2 ;;
esac
