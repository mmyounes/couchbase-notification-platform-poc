# Sync Gateway / Keycloak deployment

| Component | Address | Port | Notes |
|---|---|---|---|
| Keycloak 26.0 | `<prefix>-kc.duckdns.org` (priv `192.168.31.221`) | 8080 | Docker, volume `keycloak-data` |
| Sync Gateway 4.1.1 | `<prefix>-sg.duckdns.org` (priv `192.168.26.244`) | 4984 public, 4985 **loopback only** | 16 vCPU / 30 GB; apt package, systemd |
| Couchbase | `192.168.16.113` | 8091 | bucket `ncgr`, scope `platform` |

The two public addresses are DuckDNS names, not IPs — see "Surviving a restart"
below. Couchbase is reached on its private address, which AWS preserves across a
stop/start, so it needs no name.

## Files

- `bootstrap.json` — `/home/sync_gateway/sync_gateway.json` on the SG host.
  **`bootstrap.server` must use the `couchbase://` scheme.** An `http://` value
  is fatal at startup ("http scheme is not supported").
- `db-config.json` — the `db` database, `PUT /db/` on the admin port.
- `notifications-sync.js`, `devices-sync.js`, `import-filter.js` — embedded into
  `db-config.json`; kept as separate files so they are reviewable and diffable.

## Two settings that are load-bearing

**Keycloak `KC_HOSTNAME=http://<prefix>-kc.duckdns.org:8080`.** The `iss` claim is minted
from this. Left unset, the issuer follows the request's Host header, so a phone
authenticating over the public IP gets a different `iss` than Sync Gateway was
configured with, and every token is rejected as an issuer mismatch — which reads
like broken trust config rather than a URL problem. `db-config.json`'s
`oidc.providers.keycloak.issuer` must equal it byte for byte.

The **port is part of the issuer string**. Moving Keycloak to port 80 costs a
config PUT and a full re-import for no benefit, so leave it on 8080.

**The import filter.** The bucket holds ~514M seeded notifications. Without a
filter Sync Gateway writes a `_sync` xattr onto every one of them. The filter
restricts imports to `user1` / `user2`, the two users reserved for mobile sync.
Widening it means importing however many documents match, so widen deliberately.

## Sizing

Sync Gateway was first deployed on 2 vCPU / 3 GB and could not stay up: RSS
reached 3.1 GB against 3.7 GB of RAM, and it crash-looped with
`WaitUntilReady: unambiguous timeout`. That error points at the cluster and is
misleading — every node tested reachable on 8091 and 11210 and the cluster was
at 1.4% CPU. It was the Sync Gateway host running out of memory. Streaming a
523M-document bucket across 16 import partitions needs headroom; it now runs on
16 vCPU / 30 GB at ~3.5 GB RSS.

## Surviving a restart

The instances have no Elastic IPs, so every stop/start hands out new public
addresses. That used to be expensive rather than merely annoying: the Keycloak
address is baked into `KC_HOSTNAME`, which mints `iss`, which has to match Sync
Gateway's stored issuer — and changing that means a config PUT, which restarts
the ~524M document import scan.

Both public addresses are now DuckDNS names, so none of that happens. `iss` is
stable, Sync Gateway's config never changes, the app's compiled constants never
change, and the Keycloak container does not even need recreating — its
`--restart unless-stopped` policy brings it back with the same `KC_HOSTNAME`.

Each host repoints its own record at boot. The units and installer live in
`infra/duckdns/`, which also covers `ncgr-cb` and `ncgr-app`:

| | |
|---|---|
| Updater | `/usr/local/sbin/duckdns-update.sh <subdomain>` |
| Units | `duckdns@<prefix>-kc.timer` (Keycloak), `duckdns@<prefix>-sg.timer` (SG) |
| Schedule | `OnBootSec=30s`, then every 10 min |
| Token | `/etc/duckdns/token`, `600 root:root`, **not in git** |

The updater deliberately sends an **empty `ip=`** so DuckDNS uses the source
address of the request. That is what makes a restarted instance repair its own
record with no input; passing an explicit address would defeat it. The token is
piped to curl with `-K` so it appears neither in `ps` output nor in the journal,
which logs only `duckdns: <name> updated OK` or a failure with the response body.

Check it with `journalctl -u duckdns@<prefix>-kc.service` and
`dig +short @1.1.1.1 <prefix>-kc.duckdns.org`. Note that a resolver may answer for
DuckDNS names that were never registered, so a bare `dig` returning *something*
is not proof the record is yours — compare the address.

### The SG host resolves Keycloak privately

`/etc/hosts` on the Sync Gateway host maps `<prefix>-kc.duckdns.org` to
`192.168.31.221`, Keycloak's private address. Token validation (discovery and
JWKS) then stays inside the VPC instead of hairpinning out through the internet
gateway, and Sync Gateway keeps working even if DuckDNS is unreachable. Issuer
matching is a string comparison, so which address the name resolves to here has
no effect on it — the phone still uses public DNS.

**This overrides public DNS.** If the Keycloak instance is ever *replaced*
rather than restarted, its private address changes and every login fails while
public DNS still looks correct. Deleting the line falls back to the public path,
which also works — it was verified before the entry was added.

## What still needs a manual touch

Nothing that affects a running service. These are stale defaults in operator
scripts, all overridable by environment variable:

- `infra/watch-fts-local.sh` — `CB` and `APP`. A stale `CB` makes the loop print
  `cluster unreachable, retrying` forever, which reads as a cluster fault; a
  stale `APP` fails only at the benchmark step, after the watch completes.
- `infra/setup-cluster.sh` — `CB_HOST`. Provisioning only; not run on a restart.
- The dashboard URL you type in a browser - now `http://<prefix>-app.duckdns.org:3000`.

## Backticks break the config PUT

A backtick anywhere in a sync function or import filter — **even inside a
comment** — makes the config PUT fail with HTTP 400 and this error:

    Bad JSON: rest.ScopeConfig.Collections: struct Decode: expect }, but found _

The payload is valid JSON (verified byte-identical by sha256 on both ends, and
`json.load` accepts it), and the error's quoted context shows the backtick
rendered as `"`, which sends you looking for a quoting bug that is not there.
Use plain quotes in JS comments. This cost about an hour.

## Applying a config change without corrupting the registry

`PUT /db/_config` re-initialises the database and can take minutes. **Do not let
the client time out.** A cancelled request leaves `_sync:registry` half-written:

    [ERR] Exiting UpdateConfig - context cancelled, last error: cas mismatch
          {"document_id":"_sync:registry","bucket":"ncgr"}

The database then disappears from `_all_dbs` while the import listener keeps
running, which reads as a crash and is not one. `systemctl restart sync_gateway`
re-reads the registry and rolls back to the last committed config. Use a curl
timeout in the tens of minutes, or fire the request detached and poll
`_all_dbs`.

## What syncs to a device

Only `channel == "cblite"`. Enforced in two places:

- `import-filter-notifications.js` — keeps provider notifications out of Sync
  Gateway entirely, so the ~523M seeded email/sms/push documents are never
  imported.
- `notifications-sync.js` — returns before assigning a channel for anything that
  is not cblite. This is the enforcing copy, since channel membership is decided
  here, not by the import filter.

One event fans out to several channels but only the cblite document reaches the
phone, so an event appears once rather than three times.

**A config change restarts the 523M import backfill from sequence 0** (~2.3
hours). Until it finishes, newly written documents are queued behind the
historical scan and do not sync on their own. Force one through with:

    GET http://127.0.0.1:4985/db.platform.notifications/<url-encoded-doc-id>

That imports the document immediately. The same call on a deleted document makes
Sync Gateway return 404, but it does **not** tombstone it for connected clients —
deletions also queue behind the backfill, so a document deleted from Couchbase
keeps appearing in `_changes` with a null body until the scan reaches it.

Note `_all_docs` reads the `sg_allDocs` GSI index and lags reality; prefer
`_changes` when you want to know what a device would actually receive.

## Redeploying

    scp db-config.json ubuntu@<prefix>-sg.duckdns.org:/tmp/
    ssh ... 'curl -X PUT -u Administrator:password \
      -H "Content-Type: application/json" --data-binary @/tmp/db-config.json \
      http://127.0.0.1:4985/db/'

The first `PUT` blocks while five `sg_*` indexes build across 514M documents
(~30 min). It returns 201 when they are online; the database config persists in
the bucket and survives a restart.
