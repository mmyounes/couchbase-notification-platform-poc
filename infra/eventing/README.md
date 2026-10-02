# Eventing — `aggregate_user_devices`

Maintains a per-user device registry so push delivery can look up every device a
user owns with one KV get, instead of a query over `devices`.

| | |
|---|---|
| Source | `ncgr.platform.devices` |
| Target | `ncgr.platform.users` (binding alias `users_col`, `rw`) |
| Metadata | `ncgr.platform.eventing_metadata` |
| Function scope | `ncgr.platform` |
| Eventing nodes | `192.168.21.84`, `192.168.26.217` (co-located with FTS) |

## Files

- `aggregate_user_devices.js` — the handler, the reviewable copy.
- `aggregate_user_devices.json` — the full function definition (embeds the JS)
  posted to the Eventing API.

## Document shape

Key `usr::{tenant_id}::{user_id}`:

```json
{
  "tenant_id": "ncgr",
  "user_id": "user1",
  "updated_at": "2026-08-29T18:08:57.339Z",
  "apps": {
    "ncgrdemo": { "devices": {
      "AAAA-1111": { "platform": "ios", "model": "iPhone", "os_name": "iOS",
                     "os_version": "26.9", "registered_at": "...",
                     "push_token": "tok-...", "status": "active" }
    }}
  }
}
```

Devices are keyed by device id, so a re-registration replaces its own entry and
duplicates are impossible.

## Differences from the original function

The original read the pre-Sync-Gateway device schema. Beyond the field renames
listed at the top of the JS, three deliberate changes:

1. **Key is `usr::{tenant}::{user_id}`**, not the bare user id. Matches the
   convention every other collection uses (`dev::`, `ntf::`, `evt::`) and keeps
   two tenants sharing a user id from colliding on one aggregate.
2. **Apps nested under `apps`**, not hung off the document root. At the root, an
   app literally named `user_id` or `tenant_id` would overwrite a real field.
3. **`OnDelete` added.** The original left dead devices in the registry, which
   for a notification platform means pushes addressed to a retired token. It
   parses the identity out of the key (`dev::{tenant}::{user}::{device}`),
   because the body is already gone by then. Delete the handler if you would
   rather keep the history.

## Deploying

The API path needs the function-scope qualifiers or it answers 404
`ERR_APP_NOT_FOUND_TS`:

    Q="bucket=ncgr&scope=platform"
    E=http://192.168.21.84:8096/api/v1/functions/aggregate_user_devices

    curl -u Administrator:password -X POST "$E" \
      -H 'Content-Type: application/json' --data-binary @aggregate_user_devices.json
    curl -u Administrator:password -X POST "$E/settings?$Q" \
      -H 'Content-Type: application/json' \
      -d '{"deployment_status":true,"processing_status":true}'

Check with `GET /api/v1/status/aggregate_user_devices?$Q`.

## Reading the stats

`GET /api/v1/stats?$Q` is **per node**, and the two nodes lag each other. A node
reporting `dcp_mutation_msg_counter=0` while the other reports work is normal
mid-flight, not a stuck vbucket — sum both and re-sample before concluding
anything. Eventing is asynchronous: allow ~10-15s after a mutation before
reading the aggregate, or you will see a half-built document and misdiagnose it
as a lost update.

## Verified behaviour

- Two devices, one user, same app → both present.
- Same user across two apps → nested separately under `apps`.
- Two users → separate aggregates.
- Re-register an existing device id → updated in place, token rotated, no dupe.
- Delete a device → removed from the aggregate.
- `_sync:syncInfo` (Sync Gateway's own document in `devices`) → skipped by the
  `_sync` guard, which is why that guard has to stay.
