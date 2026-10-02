# DuckDNS self-updating records

The instances have no Elastic IPs, so every stop/start hands out new public
addresses. Each host repoints its own DuckDNS record at boot, so nothing
downstream has to be edited when that happens.

| Name | Host | Serves |
|---|---|---|
| `<prefix>-kc.duckdns.org` | Keycloak | 8080 — **also the `iss` claim**, see `../sg/README.md` |
| `<prefix>-sg.duckdns.org` | Sync Gateway | 4984 public, 4985 loopback only |
| `<prefix>-cb.duckdns.org` | Couchbase data node 1 (`192.168.16.113`) | 8091 UI and REST |
| `<prefix>-app.duckdns.org` | App server (`192.168.23.97`) | 3000 dashboard, 8080 pipeline |

`ncgr-kc` is the one that earns its keep: its address is baked into
`KC_HOSTNAME`, which mints `iss`, which must match Sync Gateway's stored issuer.
Changing that means a config PUT, which restarts the ~524M document import scan.
A stable name removes that from the restart path entirely.

## Files

- `duckdns-update.sh` — the updater, installed to `/usr/local/sbin/`.
- `duckdns@.service` / `duckdns@.timer` — systemd template units, one instance
  per subdomain (`duckdns@<prefix>-app.timer`).
- `install.sh` — installs all of the above on one host.

## Installing on a new host

    scp -r infra/duckdns ubuntu@<host>:/tmp/
    ssh ubuntu@<host> 'sudo DUCKDNS_TOKEN=$DUCKDNS_TOKEN bash /tmp/duckdns/install.sh <subdomain>'

Pass the token in the environment, not as an argument, so it stays out of that
host's shell history. It ends up at `/etc/duckdns/token`, `600 root:root`, and
is **not** in git. `install.sh` is idempotent.

## Two details that are load-bearing

**The updater sends an empty `ip=`.** DuckDNS then uses the source address of
the request, which is how a restarted instance repairs its own record with no
input. Passing an explicit address defeats the entire mechanism — do that only
from a workstation when seeding a brand-new record.

**The token is piped to curl with `-K`,** so it appears neither in `ps` output
nor in the journal. The journal gets only `duckdns: <name> updated OK`, or a
failure with the response body.

## Checking it

    journalctl -u duckdns@<prefix>-app.service
    dig +short @1.1.1.1 <prefix>-app.duckdns.org

A resolver may answer for DuckDNS names that were never registered, so a bare
`dig` returning *something* is not proof the record is yours — compare the
address against the host you expect.

## Still on bare IPs

Nothing. All four hosts have names. What remains are stale IP defaults in two
operator scripts, `infra/watch-fts-local.sh` (`CB`, `APP`) and
`infra/setup-cluster.sh` (`CB_HOST`) — both overridable by environment variable,
and both now safe to repoint at `ncgr-cb` / `ncgr-app` permanently.
