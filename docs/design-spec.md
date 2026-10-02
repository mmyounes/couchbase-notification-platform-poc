# Notification Platform — Couchbase PoC

**Design spec** · 2026-08-25 · v1.0

This document is the single source of truth for implementing the PoC. It is
self-contained: implement exactly what is described here. It supersedes any
conflicting statement in the originating requirements document, which is not
included; where this design deliberately departs from those requirements the
departure is recorded in §2.

---

## 1. Purpose and scope

### 1.1 Goal

Validate that Couchbase Server can serve as the persistence, processing,
tracking and query layer for a large notification platform, while the central
database holds approximately **500 million notification instances**.

The deliverable is a working web application that a Couchbase engineer can
demonstrate: start a sustained load, watch notifications flow through delivery
and retry, and search the resulting dataset.

### 1.2 In scope

- Event ingestion at a sustained **1,000 events/sec**.
- Template-based notification rendering, fanned out one instance per channel.
- Mock delivery channels (email, SMS, push) with controllable failure injection.
- Delivery tracking, trial counting, exponential backoff and retry.
- Flood suppression and rate limiting driven by a policy document.
- Notification search: exact filters, full-text on the message, time windows,
  and any combination — capped at 100 results per page with pagination.
- A seeder that loads 500M notification instances.

### 1.3 Explicitly out of scope (deferred to a second spec)

- Couchbase Sync Gateway configuration.
- Couchbase Lite and the mobile application.
- Device registration through IDM.
- Offline and bidirectional synchronisation tests.
- Solace (a real broker); replaced by an in-process bus — see §4.4.
- Multi-tenant administration UI; a single tenant is seeded — see §2.

The `seen` field and `sync_channels` array exist on the notification document
from day one so that the deferred work never requires migrating 500M documents.
**The server-side pipeline never writes `seen`** — it is written only by the
mobile app via Sync Gateway, and only read by this PoC.

### 1.4 Success criteria

Mapped from the originating requirements:

| Requirement | How this design satisfies it |
|---|---|
| Store and operate against ~500M notifications | Magma bucket, §10; seeder, §9.2 |
| Process 1,000 notifications concurrently | Sustained 1,000 events/sec load, §9.1 |
| Reads and inserts at acceptable latency | Per-stage histograms on the dashboard, §8.1 |
| Frequent status updates without degradation | Subdocument mutations, §6.3 |
| Track successful, pending, failed, retried | `status` + `delivery.trials`, §5.2 |
| Operational queries while processing continues | FTS search path independent of the write path, §7 |

> **Note on a contradiction in the requirements document.** Page 17 states the load test processes
> **100** notifications concurrently; page 18 states **1,000**. This PoC builds
> to a sustained **1,000 events/sec**.
> The requirements document should be reconciled at source.

---

## 2. Decisions and departures from the requirements document

| # | Decision | Rationale |
|---|---|---|
| D1 | **Rendering happens in the application, not a Couchbase Eventing function** | Makes submit→delivered a single traceable path, so per-stage latency can be attributed honestly. An Eventing hop is asynchronous and unattributable — fatal for a PoC whose only output is latency claims. Trade-off: every producer must call the API rather than writing events directly to the bucket. |
| D2 | **Both the event and the notification are persisted** | Gives an audit trail, replay capability, and the record for suppressed deliveries (D5). Adds 1,000 inserts/sec on top of the 3,000/sec the notifications already require — see the throughput budget in §6.5. |
| D3 | **Single tenant, `tenant_id` present on every document and key** | The requirements document treats multi-tenancy as core, but tenant CRUD exercises no Couchbase capability. Keeping the field and key prefix means the 500M dataset never needs migrating if real tenancy is added. |
| D4 | **Status vocabulary is exactly `PENDING \| DELIVERED \| FAILED`** | Follows the requirements document: a failed attempt leaves status at `PENDING` and increments `trials`; only exhausting `max_attempts` sets `FAILED`. No invented `RETRYING` state. "Retried" is expressed as `trials > 0`. |
| D5 | **Suppressed messages create no notification document** | Suppression happens before fan-out. The verdict is recorded on the *event* document, which keeps `notifications.status` free of invented values and is the concrete payoff for D2. |
| D6 | **Deduplication keys on `tenant_id + user_id + event_type + channel`, never on message content** | Content hashing fails outright for this PoC's primary template: every password reset carries a different `temp_password`, so a content hash differs every time and flood control never fires. The requirements document's own policy names ("Max per Receiver/Hour", "Max per Channel/Min") are receiver- and channel-scoped, not content-scoped. |
| D7 | **A single global policy document**, not per-tenant | Retry and duplicate policy are identical for all users in this design. Keyed as `policy::global`; a per-tenant lookup with fallback can be added without schema change. |
| D8 | **Rate and flood counters are atomic KV counters with TTL, never queries** | Sub-millisecond, self-expiring, no index, and indifferent to whether the bucket holds 500 or 500M documents. |
| D9 | **Durability is `none` on all writes** | Chosen for this PoC. No configurable durability knob. |
| D10 | **FTS is the search backend; GSI serves only fixed-shape queries** | Arbitrary filter combinations across seven fields would need a combinatorial explosion of secondary indexes. FTS is built for that shape. |
| D11 | **Message bodies are indexed unredacted** | Chosen for this PoC. Consequence: the FTS dictionary carries ~500M unique terms from temp passwords (~16 GB overhead) and the index contains credentials. Acceptable for a PoC on synthetic data; would be a finding in production. |
| D12 | **Solace is replaced by an in-process bus** behind an interface using the requirements' topic names | Keeps focus on Couchbase. Swapping in a real broker is one implementation of one interface. |
| D13 | **`_type` is the FTS `type_field`** | *Verified defect fix.* Pointing it at `type` resolves documents to `platform.notifications.password_reset`, which never matches the mapping key — the index builds successfully over 500M documents and returns zero results for every query. `_type` exists on no document, so the type resolves to `platform.notifications`. |

---

## 3. Technology

- **Language:** **Go for the backend, TypeScript for the UI.** The pipeline and
  seeder are Go (`gocb/v2`); the web application is TypeScript. This reverses
  the original decision — see §10.1c. In short: Node executes JavaScript on one
  core, which forced horizontal replication onto a single-box PoC, and every
  operational defect encountered came from that replication rather than from the
  business logic. Goroutines use all sixteen cores from one process.
- **UI:** Next.js (App Router), React, Tailwind CSS, **shadcn/ui**. Components
  are copied into the repo rather than imported from a package, so restyling
  later is editing your own file. Recharts for charts, TanStack Query for
  polling.
- **Services:** Fastify. Chosen over NestJS for less ceremony when iterating.
- **Couchbase SDK:** `gocb/v2` in the pipeline and seeder.
- **Services:** Go standard-library HTTP for the pipeline; Fastify for
  mock-channels, which does no Couchbase work.
- **Metrics:** `hdrhistogram-go`, one registry for the whole service.
- **Monorepo:** pnpm workspaces for the TypeScript packages; the Go services are
  self-contained modules.
- **Shared types are duplicated** between `packages/shared` (TypeScript, used by
  the UI) and `apps/pipeline-go/types.go`. Two definitions of the same document
  shapes is a real cost of the split and they must be kept in step by hand.

### 3.1 Repository layout

```
apps/web              Next.js UI + runtime API proxy
apps/pipeline-go      Go: ingest API, workers, scheduler, load generator
apps/mock-channels    Fastify: email/sms/push simulators
apps/loadgen          sustained-rate driver (own process, §9.1)
apps/seeder-go        500M dataset loader (Go, see §9.2)
packages/shared       document types, key builders, policy schema, cursor codec
packages/couchbase    connection, typed collection handles, index DDL
infra/                docker-compose, setup-cluster.sh, fts-notifications.json
docs/                 this spec
```

`packages/shared` is load-bearing: document shapes and key-construction
functions are defined once and imported by every other package, so the UI
cannot drift from what the pipeline writes.

---

## 4. Architecture

### 4.1 Deployment

One EC2 instance runs the entire application layer. Couchbase runs on seven
separate nodes (§10). Services are **separate processes on that one box**,
orchestrated by `docker compose`:

```
┌─ EC2 (application layer) ─────────────────────────────┐
│  web             Next.js UI                            │
│  pipeline        Event Processor · Tracking Manager ·   │
│                  Retry Scheduler · Throttle Guard       │
│  mock-channels   email / sms / push simulators          │
│  loadgen         sustained-rate driver (own process)    │
└────────────────────────────────────────────────────────┘
                    │ Couchbase Node SDK
                    ▼
        Couchbase cluster — 7 nodes (separate)
```

Separate processes, not one: Node executes JavaScript on a single core, so one
process on a scaled-up instance would use a fraction of it and the load test
would measure a deployment artefact. Critically, `mock-channels` must not share
an event loop with the Tracking Manager — its injected latency would otherwise
contaminate the very measurements the PoC exists to produce.

### 4.2 Components

| Component | Responsibility |
|---|---|
| **Event Processor** | Validate, resolve event definition, apply policy, render, persist event + notifications, publish delivery commands |
| **Channel Adapter** | Consume delivery commands, call the channel API, publish results |
| **Tracking Manager** | Consume results, update notification status and trial count, increment dashboard counters |
| **Retry Scheduler** | Poll the partial index for due retries, republish delivery commands |
| **Throttle Guard** | Evaluate dedup and rate caps against KV counters |
| **Mock Channels** | Simulate email/SMS/push with controllable latency and failure |

### 4.3 Boundaries

Each component exposes a narrow interface and can be tested independently. The
Throttle Guard is a pure function over `(policy, counters)` plus KV increments;
the Tracking Manager is a pure function from `(result, policy, current state)`
to a mutation set. Both are unit-testable without Couchbase.

### 4.4 The bus

```ts
interface Bus {
  publish(topic: string, msg: unknown): Promise<void>
  subscribe(topic: string, handler: (msg: unknown) => Promise<void>): void
}
```

Topics use the requirements' names: `notification.delivery.{channel}` and
`notification.results.{channel}`. The PoC implementation is an in-process
bounded queue with configurable concurrency per topic. A Solace implementation
would satisfy the same interface with no change above it.

---

## 5. Data model

Bucket `ncgr`, scope `platform`, eight collections mapping onto the requirements' four
persistence domains.

| Requirement domain | Collections | Key pattern |
|---|---|---|
| Configuration Data | `tenants`, `event_defs`, `templates`, `policies` | `tnt::{t}`, `evd::{t}::{event_type}`, `tpl::{t}::{template_id}`, `policy::global` |
| Policy State | `counters` | `dd::…`, `rc::…` (TTL) |
| Notification Data | `events`, `notifications` | `evt::{t}::{event_id}`, `ntf::{t}::{event_id}::{channel}` |
| Device Management | `devices` | `dev::{t}::{user_id}::{device_id}` |

Collections provide type discrimination, so documents carry no `doc_type`
field. This is what allows `type` to keep the business meaning the originating requirements'
original JSON gave it.

### 5.1 `events`

Key fields only — **never the rendered message**.

```json
{
  "event_id": "01K5Z8FQ3M7V8XKQ2R4T6Y9WBC",
  "tenant_id": "ncgr",
  "user_id": "user1",
  "app_name": "ncgrdemo",
  "type": "password_reset",
  "template_id": "reset_password",
  "params": { "app_name": "ncgrdemo", "temp_password": "Xk7-92Qa", "expiry_minutes": 15 },
  "submitted_at": "2026-08-25T10:30:00Z",
  "source": "load_test",
  "channels_requested": ["email", "sms", "push"],
  "channels_accepted": ["email", "push"],
  "suppressed": { "sms": "identical_cap" }
}
```

`channels_requested` is what the event definition resolved to.
`channels_accepted` is the subset that passed the policy checks — known at
step 3 of §6.1, before the event is written, so both fields are final at insert
time and the event document is written exactly once.

`suppressed` is present only when at least one channel was blocked (D5), and
maps each blocked channel to the rule that blocked it. It is the audit record
for deliveries that deliberately did not happen. `channels_accepted` and the
keys of `suppressed` are disjoint, and together equal `channels_requested`.

Written with **`insert`**, not `upsert`, so a replayed `event_id` is rejected —
idempotency with no extra machinery.

### 5.2 `notifications`

One document per channel. Each is tracked independently.

```json
{
  "tenant_id": "ncgr",
  "event_id": "01K5Z8FQ3M7V8XKQ2R4T6Y9WBC",
  "user_id": "user1",
  "app_name": "ncgrdemo",
  "type": "password_reset",
  "channel": "email",
  "subject": "Password reset for ncgrdemo",
  "message": "Your temporary password for ncgrdemo is Xk7-92Qa. It expires in 15 minutes.",
  "template_id": "reset_password",
  "template_version": 3,
  "timestamp": "2026-08-25T10:30:00.000Z",
  "status": "PENDING",
  "seen": false,
  "delivery": {
    "trials": 0,
    "last_attempt_at": null,
    "next_attempt_at": null,
    "last_error": null,
    "delivered_at": null
  },
  "sort_key": "01K5Z8FQ3M7V8XKQ2R4T6Y9WBC::email",
  "sync_channels": ["tenant::ncgr::user::user1"]
}
```

**Field notes**

- `status` — `PENDING | DELIVERED | FAILED` only (D4).
- `timestamp` — **millisecond precision, exactly equal to the time embedded in
  the `event_id` ULID.** This is a correctness requirement, not a formatting
  preference. With second-level truncation a document stamped `10:30:00Z` can
  carry a ULID for `10:30:00.750`, so `ulidCeil("10:30:00")` sorts below it and
  a `sort_key` range query silently drops boundary documents. *Verified on the
  live cluster: over the same window, a `sort_key` range and a `timestamp`
  range both return 147 of 500 documents, and FTS returns 147 as well.*
- `seen` — user-interaction state, distinct from delivery state as the requirements document
  requires. Written only by the mobile app; never by this pipeline.
- `sort_key` — `{event_id}::{channel}`. See §7.2.
- `sync_channels` — present now so Sync Gateway needs no migration later.
- `template_version` — which template version produced this text.

### 5.3 `templates`

Per-channel variants, because the channels genuinely differ.

```json
{
  "template_id": "reset_password",
  "tenant_id": "ncgr",
  "version": 3,
  "variants": {
    "email": {
      "subject": "Password reset for {{app_name}}",
      "body": "Your temporary password for {{app_name}} is {{temp_password}}. It expires in {{expiry_minutes}} minutes."
    },
    "sms":  { "body": "{{app_name}} temp password: {{temp_password}} (valid {{expiry_minutes}}m)" },
    "push": { "title": "Password reset", "body": "Your temporary password for {{app_name}} is ready. Open the app to view it." }
  },
  "params": ["app_name", "temp_password", "expiry_minutes"]
}
```

The push variant deliberately omits the password — push bodies render on lock
screens. That asymmetry is the argument for per-channel templates.

Substitution is `{{param}}` replacement implemented in `packages/shared`. No
templating dependency. A missing parameter is an error, not an empty string.

### 5.4 `event_defs`

Configuration that decides fan-out.

```json
{
  "tenant_id": "ncgr",
  "event_type": "password_reset",
  "channels": ["email", "sms", "push"],
  "template_id": "reset_password",
  "priority": "critical"
}
```

### 5.5 `policies` — key `policy::global`

```json
{
  "retry": {
    "max_attempts": 3,
    "backoff": { "initial_ms": 1000, "multiplier": 2, "max_ms": 30000, "jitter": true }
  },
  "throttle": {
    "max_identical_per_user_per_minute": 10,
    "max_per_user_per_hour": 100,
    "max_per_channel_per_minute": 50000
  }
}
```

Loaded into memory at startup, refreshed on a configurable interval (default
10s) so the hot path performs zero policy reads. Editable from the UI; the next
refresh applies it.

### 5.6 `counters`

Binary counters, atomic `increment`, explicit per-document TTL. The collection
carries `maxTTL: 7200` as a backstop so stale counters cannot accumulate.

| Key | Window | TTL |
|---|---|---|
| `dd::{t}::{user}::{event_type}::{channel}::{yyyymmddhhmm}` | identical-message, per minute | 120s |
| `rc::{t}::{user}::{yyyymmddhh}` | per user, per hour | 7200s |
| `rc::{t}::ch::{channel}::{yyyymmddhhmm}` | per channel, per minute | 120s |

Dashboard counters (`submitted`, `delivered`, `failed`, `retried`, `suppressed`,
`pending`) also live here, without TTL, keyed `stat::{t}::{name}`.

### 5.7 `devices`

Schema defined, populated by the deferred mobile work. Fields per the requirements document:
`user_id`, `tenant_id`, `device_id`, `platform`, `push_token`, `status`,
`metadata`.

### 5.8 `users` — device registry aggregate

Maintained by the `aggregate_user_devices` Eventing function on every `devices`
mutation, so push delivery resolves a user's devices with one KV get rather than
a query. Key `usr::{tenant_id}::{user_id}`; devices nested under
`apps.{app_name}.devices.{device_id}`, so a re-registration replaces its own
entry and duplicates cannot occur.

Written **only** by Eventing. Nothing else writes this collection.

### 5.9 `eventing_metadata`

Eventing's checkpoint and state store. Must exist before any function
referencing it deploys, and must not be read or written by any handler.

See `infra/eventing/README.md` for the handler, its differences from the
original function, and the deployment procedure.

---

## 6. Processing flow

### 6.1 Happy path

```
loadgen / UI ──> Event Processor
  1. resolve tenant + event definition        (in-memory cache, no I/O)
  2. dedup check    dd::…                     (KV incr)
  3. rate checks    rc::…                     (KV incr)
  4. INSERT event document                    (insert → replays rejected)
  5. per surviving channel:
       render template variant
       INSERT notification (PENDING, trials 0)
       publish notification.delivery.{channel}
                             │
Channel Adapter ─────────────┘  POST mock-channels/{channel}/send
       publish notification.results.{channel}
                             │
Tracking Manager ────────────┘
       success → status=DELIVERED, delivered_at, trials+1
       failure → trials+1
                 if trials ≥ max_attempts → FAILED + last_error
                 else                     → stays PENDING,
                                            next_attempt_at = backoff(trials)
       increment stat:: counters
                             │
Retry Scheduler (1s tick) ───┘
       SELECT … WHERE status='PENDING'
                 AND delivery.next_attempt_at
                     BETWEEN (NOW - horizon) AND NOW   LIMIT n
       republish notification.delivery.{channel}
```

**The retry query must be bounded at BOTH ends.** An unbounded
`next_attempt_at <= NOW` is correct on an empty cluster and catastrophic on the
real one: a 500M-document historical dataset holds millions of `PENDING`
notifications whose `next_attempt_at` is months past, so all of them look due
forever. *Observed on the live cluster — the scheduler re-delivered roughly
28,000 seeded historical notifications, mutating their status and trial counts,
before the bound was added.* `RETRY_HORIZON_MS` defaults to one hour; bounding
both ends keeps this a pure range scan on `idx_retry`.

**Reconciliation is free, not a second query.** The Event Processor stamps
`next_attempt_at = now + STALE_AFTER_MS` when creating a notification. Normal
delivery overwrites that field long before it comes due; a notification whose
delivery command was lost to a restart is still `PENDING` when the grace expires,
and the same retry query recovers it. An earlier design used a separate
stale-pending sweep on `next_attempt_at IS NULL`, which matched essentially every
seeded historical document and cannot be served by the partial index.

### 6.2 Order of operations

Policy checks precede rendering and persistence. If every requested channel is
suppressed, the event document is still written (with `suppressed` populated)
and no notification is created.

Rendering happens before the notification insert; render cost is microseconds
and a linear flow is clearer than a conditional one.

### 6.3 Tracking updates use subdocument mutations

The Tracking Manager **never** reads and rewrites a whole notification. It
issues a `mutateIn` with an atomic `increment` on `delivery.trials` plus
targeted path sets. Consequences:

- The document body never crosses the network.
- No lost-update race, so no CAS retry loop.
- At 500M documents and thousands of updates per second, this is the difference
  between a good result and a bad one.

This is a **hard requirement**, not an optimisation. Any implementation that
does `get` → mutate → `replace` is wrong.

### 6.4 Retry and failure classification

Backoff is `min(initial_ms × multiplier^(trials-1), max_ms)` with jitter.
With the default policy: ~1s, ~2s, ~4s.

Failures are classified, because retrying a malformed request three times is a
bug:

| Channel response | Treatment |
|---|---|
| `5xx`, timeout, connection error | Retryable — backoff, `trials+1`, stays `PENDING` |
| `4xx` | Permanent — straight to `FAILED`, no further attempts |

### 6.5 Throughput budget

At the target rate of 1,000 events/sec, with a three-channel event definition
and 10% injected failure:

| Operation | Type | Rate |
|---|---|---|
| Event inserts | KV `insert` | 1,000/sec |
| Notification inserts | KV `insert` | 3,000/sec |
| Throttle counters (3 per event) | KV `increment` | 3,000/sec |
| Tracking updates, first attempt | KV `mutateIn` | 3,000/sec |
| Retry attempts + their tracking updates | KV `mutateIn` | ~600/sec |
| Dashboard stat counters | KV `increment` | ~3,000/sec |
| Retry scheduler poll | N1QL, `idx_retry` | 1/sec |
| **Total** | | **~13,600 KV ops/sec** |

All of it is KV or subdocument work against a 150 GB quota on three data nodes.
No operation in the steady-state write path touches a query or an index on the
500M collection — the only recurring query is the retry scheduler's one-per-
second scan of a partial index holding thousands of entries.

This is the number to state when reporting results, and the per-stage
histograms on the dashboard (§8.1) are what attribute it.

### 6.6 Suppression semantics

Two independent mechanisms with distinct UI outcomes:

- **Flood suppression** — same user, same event type, same channel, more than
  `max_identical_per_user_per_minute`. Never reaches a channel. Recorded on the
  event as `suppressed: { channel: "identical_cap" }`.
- **Delivery retry exhaustion** — the channel rejected it `max_attempts` times.
  Notification reaches `FAILED` with `last_error`, and the UI reports delivery
  unsuccessful.

These must look different on screen. Conflating them into one "ignored" state
hides which rule fired.

---

## 7. Search and indexing

The governing constraint: **no user-facing query may do work proportional to
500M**.

### 7.1 Query paths

| Need | Backend |
|---|---|
| Latest feed, user lookup, retry pickup | GSI |
| Any filter combination, full-text, time windows | FTS |
| Dashboard totals | KV counters, never `COUNT(*)` |

Covering arbitrary combinations of `user_id`, `app_name`, `channel`, `status`,
`type`, `seen` and time window with GSI would require a combinatorial explosion
of indexes. FTS handles it in one index.

### 7.2 Pagination — keyset, never offset

`OFFSET 900` makes the engine scan and discard 900 rows; FTS `from`/`size` deep
paging degrades worse. Instead every notification carries:

```
sort_key = "{event_id}::{channel}"
```

`event_id` is a **ULID** — lexicographically sortable, time-ordered, and
unique. Appending the channel makes it unique across fan-out. One string field
therefore provides the sort order, the pagination cursor and the tiebreaker.

```sql
SELECT … FROM notifications
WHERE tenant_id = $t AND sort_key < $cursor
ORDER BY sort_key DESC LIMIT 100
```

FTS uses `search_after` on the same field, so both paths share one cursor
format and the UI has one pagination component. **Verified on 8.0.2:**
`search_after` returns correct descending order across pages, reports
`total_hits` on every page, and echoes each hit's sort value so the client
carries the cursor rather than constructing it.

Because ULIDs are time-ordered, a **time-window filter is a range on
`sort_key`** (`ulid_floor(from)` … `ulid_ceil(to)`), served by the same index.

> **Seeder requirement.** The 500M seeded documents must be given ULIDs whose
> embedded timestamp matches their synthetic `timestamp`. Otherwise seeded
> ordering does not match real ordering and every assumption above breaks.

### 7.3 FTS returns IDs, not documents

The index stores no field bodies. A search returns 100 document IDs plus
`total_hits`; a single bulk KV `get` then fetches those 100 documents. This
keeps the index materially smaller, makes a real result count free, and a
100-key multi-get is the cheapest operation Couchbase performs.

### 7.4 Indexes as built

| Index | Definition | Scale |
|---|---|---|
| `idx_multi` | `notifications(tenant_id, sort_key DESC, status, channel, app_name, type, seen, user_id)` | 514M |
| `idx_user_multi` | `notifications(tenant_id, user_id, sort_key DESC, status, channel, app_name, type, seen)` | 514M |
| `idx_retry` | `notifications(delivery.next_attempt_at)` **`WHERE status='PENDING'`** | thousands |
| `idx_evt_suppressed` | `events(tenant_id, submitted_at DESC)` **`WHERE suppressed IS NOT MISSING`** | small |
| `fts_notifications` | 8 partitions, 1 replica — see below | 500M |
| `#primary` | on `tenants`, `event_defs`, `templates`, `policies`, `devices` only | tiny |

**No primary index on `notifications`, `events` or `counters`.** At 500M
documents it is a footgun, and every query above is covered.

**Two composites, not one, and not four.** Both lead with `tenant_id` and then
diverge on whether `user_id` precedes the `sort_key` range. That split is forced
by how GSI composites work: a range predicate on key *N* demotes keys *N+1…* to
post-filters. `idx_multi` ranges on `sort_key` at key 2, so a trailing `user_id`
is only a post-filter and a single-user query degenerates into a scan of the
whole tenant — **measured at 49.3 s**. `idx_user_multi` places `user_id` at key 2,
ahead of the range, making it a seek: **1.6 ms** for the same query and rows.
Merging them back into one index re-creates the 49-second path.

Two narrower indexes were **dropped** once the composites were online, each an
exact key prefix of a composite and therefore incapable of answering anything
its successor could not:

| Dropped | Superseded by | Disk recovered (per index node) |
|---|---|---|
| `idx_feed` `(tenant_id, sort_key DESC)` | `idx_multi` | 30.1 GB / 28.9 GB |
| `idx_user` `(tenant_id, user_id, sort_key DESC)` | `idx_user_multi` | 32.3 GB / 33.0 GB |

Verified across 19 query shapes after dropping: every shape kept an index seek
and its `index_order` (no `Order` operator anywhere, so ordering still comes from
the index rather than a sort), and 18 of 19 ran in 1.4–2.9 ms. The only cost was
the unfiltered dashboard feed at 1.6 → 1.7 ms. Rebuild cost if this proves wrong
is ~50 min per composite over 514M items.

Do not re-add either dropped index. `infra/setup-cluster.sh` carries the same
warning, because a fresh provisioning run is the likely way they come back.

All large indexes are created with `defer_build: true` and built **after**
seeding. Maintaining them across 500M inserts is dramatically slower.

**FTS mapping** — type `platform.notifications`, `doc_config.mode =
scope.collection.type_field`, `type_field = _type` (D13):

| Field | Type | Analyzer | Docvalues |
|---|---|---|---|
| `user_id`, `app_name`, `channel`, `status`, `type`, `tenant_id` | text | keyword | no |
| `subject`, `message` | text | standard | no |
| `timestamp` | datetime | — | yes |
| `seen` | boolean | — | no |
| `sort_key` | text | keyword | yes |

Docvalues are enabled only where sorting or ranging requires them.

---

## 8. UI

Four pages: **Dashboard · Send · Notifications · Configuration**.

### 8.1 Bulk Load

> Route `/load`, second in the nav. Named *Dashboard* until the tiles were
> reordered; the whole-collection totals it used to carry now live on §8.3,
> above the filters, where the result counts they contextualise are.

The load console and the page to demo from.

```
┌─ Load Console ──────────────────────────────────────────────┐
│ Rate [1000]/s   Duration [60]s   Failure injection [10]%    │
│ User pool [100k]   Channels [✓email ✓sms ✓push]             │
│                              [ ▶ Start ]   [ ■ Stop ]       │
└─────────────────────────────────────────────────────────────┘
┌ Submitted ─┬ Pending ┬ Delivered ─┬ Failed ┬ Retried ┬ Suppressed ┐
└────────────┴─────────┴────────────┴────────┴─────────┴────────────┘
┌─ Throughput, 60s ───────────┐ ┌─ End-to-end latency p50/95/99 ─┐
┌─ Per-stage p95 (µs): policy · render · event insert ·          │
│  ntf insert · channel call · tracking mutateIn · retry pickup  │
└────────────────────────────────────────────────────────────────┘
```

Counters read six keys per second regardless of dataset size. The per-stage row
separates Couchbase latency from the mock channel's simulated latency — the
distinction that makes the reported numbers defensible.

Polled at 1s via TanStack Query.

### 8.2 Send

Single-message console. Compose panel on the left (user, app, event type,
channel checkboxes, per-channel force-failure toggles, repeat count), live
trace on the right showing every stage with timestamps: policy verdicts, event
persistence, then per channel the notification creation, each trial with its
result and backoff wait, and the terminal outcome.

Setting *Repeat* above the policy cap shows later sends rejected as
`suppressed: identical_cap (11/10)` — visibly distinct from retry exhaustion
above it. This page is where §6.6's two mechanisms are demonstrated.

### 8.3 Dashboard (notification search)

> Route `/notifications`, first in the nav and the page the demo opens on —
> the bare origin 307-redirects here (`next.config.mjs`), so there is no page
> at `/`.
> Carries the collection-wide totals (total / delivered / failed / pending, via
> FTS, refreshed every 30s) above the filter bar: they are the denominator for
> every filtered count below them.

One page, not two. Filter bar (time window, user, app, channel, status, type,
seen, plus a full-text box), results table capped at **100 rows** with keyset
pagination, and a row-click drawer showing the full document, every delivery
attempt, the template version and `sync_channels`.

**The full-text box must send `operator: "and"`** (or a phrase query). FTS
defaults a multi-term `match` to OR, so searching *"temporary password"* also
returns documents containing only *"password"* — verified on seeded data, where
it returned 157 hits spanning two unrelated template types instead of the 144
`reset_password` instances. Defensible as relevance ranking, but wrong for a
filter box the user reads as a conjunction.

With no filters applied it is the **latest-notifications feed**, auto-refreshing
every 2s. Applying a filter or advancing a page **pins the cursor and stops
auto-refresh** — a live-updating result set cannot be paged through. A
`● Live` / `⏸ Paused` badge makes the mode visible.

A collapsed disclosure at the foot of the page reveals **the query the server
actually ran** for the current result set, so the latency figure beside it is
checkable rather than asserted. The GSI path shows one N1QL statement with its
literals inlined — it pastes straight into the query workbench. The text path
shows two entries, because it is two round trips: the FTS request body, then the
KV multi-get that hydrates the returned ids. The FTS body also exposes the
implicit `DEFAULT_TEXT_WINDOW_MS` bound, which the user never types and would
otherwise have no way to see.

Literals are inlined in a single token-scan pass, not by successive
`ReplaceAll`: a value containing `$to` would otherwise be corrupted by a later
substitution, and short parameter names would match inside longer ones.

### 8.4 Configuration

Two tabs. *Policy* edits `policy::global` and takes effect at the next cache
refresh, so max attempts can be changed mid-demo. *Templates* edits variants
with a live preview.

---

## 9. Load generation and seeding

### 9.1 Load generator

A dedicated worker process. Token-bucket limiter at the target rate, drawing
users from a configurable pool. Failure injection is a percentage of events
flagged for the mock channel to reject, split between retryable `5xx` and
permanent `4xx`. Reports achieved rate, and per-stage HDR histograms via
`/metrics`.

### 9.2 Seeder

Loads 500M notification instances. **Notifications only** — events are created
by live traffic; seeding them would add ~167M documents and satisfy no success
criterion.

Requirements:

- Batched KV inserts across worker processes, durability `none`.
- **ULIDs backdated to match each document's synthetic `timestamp`** (§7.2).
- Users drawn from a **Zipf** distribution, so some users are genuinely hot.
- Status distribution ≈ 85% `DELIVERED`, 10% `FAILED`, 5% `PENDING`.
- **6–8 distinct template types** with genuinely varied content. If all 500M
  documents carry near-identical text, a search for *"temporary password"*
  matches everything and the search demo proves nothing.
- Progress reporting with achieved ops/sec and ETA; resumable.
- Indexes stay deferred throughout; build afterwards.
- **Blind retry on any KV failure, up to `--max-retries`.** Upserting identical
  content is idempotent, so retrying is always safe — including after an
  *ambiguous timeout*, where the first attempt may already have landed. This is
  not optional: an observed 1-in-500 transient failure rate becomes roughly a
  million lost documents at 500M scale.
- Default `--kv-timeout-ms 10000`. The SDK default of 2.5s produced an ambiguous
  timeout during connection warm-up on a 500-document run.

**Status:** implemented as `apps/seeder-go`, verified against the live cluster.
A 500-document run wrote 500/500 with 0 failures at 70,190 ops/sec on warm
connections, and the resulting distribution in Couchbase matched the dry-run
prediction exactly (DELIVERED 421 / FAILED 52 / PENDING 27, all eight template
types, zero `user1`/`user2`). `--dry-run` asserts the ULID/timestamp invariant
across the whole sample and reports mismatches.

### 9.3 Mock channels

Per-channel endpoints with configurable base latency and jitter. Failure is
driven either by a per-request header (single-message console) or a global
injection rate (load test). Returns `503` for retryable failures and `400` for
permanent ones.

---

## 10. Cluster (as built and verified)

**Couchbase Server 8.0.2 Enterprise**, 7 nodes, `r7i` class, gp3 EBS.

| Role | Count | vCPU | RAM | Disk | Service quota/node |
|---|---|---|---|---|---|
| Data | 3 | 8 | 61.6 GB | 483 GB | 51,200 MiB |
| Index + Query | 2 | 16 | 123.5 GB | — | 98,304 MiB |
| Search | 2 | 16 | 123.5 GB | — | 102,400 MiB |

**Bucket `ncgr`** — verified: `storageBackend=magma`,
`evictionPolicy=fullEviction` (required by Magma), `numVBuckets=1024`,
`replicaNumber=1`, `compressionMode=active`, `conflictResolutionType=seqno`,
`durabilityMinLevel=none`, quota 51,200 MiB/node = 153,600 MiB cluster.

`storageBackend` and `numVBuckets` are **immutable after creation**.

### 10.1 Sizing analysis

Notification ≈ 750 bytes + 45-byte key + 56 bytes metadata.

| | |
|---|---|
| 500M logical | ~375 GB |
| With 1 replica | ~750 GB |
| Bucket quota | 150 GB |
| **Resident ratio** | **~20%** |

Magma is mandatory at this ratio; Couchstore expects metadata essentially
resident and does not behave well at 167M items per node.

**Index:** `idx_feed` ~75 GB + `idx_user` ~87 GB = 162 GB primary, 324 GB with
replica, against ~192 GB of index quota — roughly 60% resident. Acceptable
because both queries are keyset scans anchored at the most recent end, so
access concentrates in a small hot region. A random historical user lookup will
hit cold pages and cost a disk read.

> **Superseded — planning estimate, and wrong by ~2.4x.** Measured disk was
> 32.0 GB and 34.7 GB, not 75 GB and 87 GB (§10.1e: Plasma compresses ~3x, which
> this estimate did not account for). Both indexes were later dropped as
> redundant (§7.4). The index set that actually matters is `idx_multi` (41.7 GB)
> + `idx_user_multi` (44.4 GB) + `idx_retry` (1.5 GB) ≈ 88 GB per index node.
> The reasoning about hot-region concentration held up and still applies.

**Search:** ~80–110 GB primary, 160–220 GB with replica, against ~200 GB of
search quota. **This is the tightest-fitting component**; a third search node
is the first thing to add. Unredacted temp passwords (D11) contribute ~16 GB of
single-posting dictionary entries.

### 10.1a Measured results at 500M (supersedes the estimates above)

The dataset was loaded and measured on the live cluster. Where measurement
disagrees with §10.1, **the measurement is authoritative**.

**Load:** 500,000,000 notifications in 6,298s (~105 min) at 79,386 ops/sec
average, 0 failures, 1 retry. Written by `apps/seeder-go` at 320 workers, with
all GSI indexes building and FTS ingesting concurrently.

| | Estimated | Measured |
|---|---|---|
| Data on disk (incl. replica) | ~750 GB | **376.9 GB** (754 B/doc) |
| Data per node | ~250 GB | **~126 GB** (29% of 483 GB) |
| FTS index on disk | 80–110 GB | **269.5 GB** |

Compression roughly halved the replicated data, so the data tier is far more
comfortable than planned. **The FTS estimate was wrong by ~2.7x** and is the
number to carry into any production sizing.

**Query latency** (median of 3, each verified to return 100 real rows):

| Query | Matching docs | Latency |
|---|---|---|
| Latest feed, page 1 (GSI `idx_feed` — since dropped, now `idx_multi`) | — | **7 ms** |
| User lookup (GSI `idx_user` — since dropped, now `idx_user_multi`) | — | **6 ms** |
| Text + 1-day window (FTS) | 162,600 | 1,356 ms |
| Text + channel, 7-day window (FTS) | 759,954 | 3,268 ms |
| Text, 7-day window (FTS) | 2,507,904 | 4,197 ms |
| `status=FAILED`, unscoped (FTS) | 50,007,099 | 4,359 ms |
| Text over full 12 months (FTS) | 139,269,771 | 24,971 ms |

**The GSI path meets any reasonable latency bar; the FTS path does not, and the
cost tracks the size of the matching set rather than the corpus.**

**Why FTS is slow here: partition parallelism, not memory and not disk.**

Measured on a search node during a 139M-hit query:

```
 r  b     free      cache    bi  bo   us sy id wa
 4  0  1152560  126002144    4   0   26  1  74  0
 4  0  1151624  126002144    0   0   26  0  74  0
```

- `bi = 0` — **zero blocks read from disk**, `wa = 0` — no I/O wait.
- `/proc/meminfo`: `Cached` = 117.7 GB per node, `MemFree` = 1.35 GB.
- `us = 26%` of 16 vCPU ≈ 4.2 cores, with a **run queue of exactly 4**.

The index is comfortably page-cached and the query never touches disk. FTS runs
**one thread per index partition**, and with `indexPartitions: 8` plus one
replica each node holds exactly 4 *active* partitions (verified in
`planPIndexes`: 4 at priority 0, 4 at priority 1 per node). So a query can use
at most 4 of 16 cores — a hard 25% ceiling, which is precisely what the 26%
measurement shows. Each partition scans 500M / 8 = 62.5M documents and the
slowest one sets the latency.

| | `indexPartitions: 8` | `indexPartitions: 32` |
|---|---|---|
| Active partitions per node | 4 | 16 |
| Cores usable per query | 4 of 16 | 16 of 16 |
| Documents per partition | 62.5M | 15.6M |

**Raising `indexPartitions` is the fix.** It requires a full index rebuild
(the value is fixed at creation), and 16 active partitions on 16 vCPU leaves no
headroom for merges and concurrent ingest, so 24 (12 per node) may behave better
under write load. Reducing the index size also helps — reconsider D11, since
unredacted temp passwords contribute ~500M single-posting dictionary entries to
the 269.5 GB.

**Outcome of the repartition (32 partitions, `sort_key` and `timestamp`
docvalues removed).** Verified: `r=16`, `us=98%`, `bi=0` — all 16 cores
saturated, still zero disk reads. Index shrank 269.5 GB -> **221.7 GB**.

| Query | 8 partitions | 32 partitions (warm) | Gain |
|---|---|---|---|
| Latest feed / user lookup (GSI) | 6-7 ms | **6 ms** | unchanged |
| Text + 1-day window | 1,020 ms | **289 ms** | 3.5x |
| Hybrid text + channel | 2,331 ms | **600 ms** | 3.9x |
| `status=FAILED` (50M hits) | 2,537 ms | **713 ms** | 3.6x |
| Text, 7-day window (2.4M hits) | 2,965 ms | **758 ms** | 3.9x |
| Text, 12 months (139M hits) | 21,735 ms | **5,404 ms** | 4.0x |

**Parallelism is `min(partitions, cores)`.** At 8 partitions on 32 cores the
partitions bound it; at 32 partitions on 32 cores the two are matched and the
nodes run at 98%. A third search node would now help — but only if partitions
rise with it, since 32 partitions on 48 cores is still 32 threads. Scale the two
together or neither.

**A non-selective date predicate is pure overhead.** Measured warm, direct to a
search node:

| | Hits | Latency |
|---|---|---|
| No date predicate | 139,269,771 | 2,373 ms |
| \+ 12-month range (matches everything) | 139,269,771 | **5,378 ms** |
| \+ 1-day range | 380,898 | **409 ms** |

A range covering the whole dataset **more than doubles** the query while
returning the identical result set: a bleve datetime range expands into many
prefix-coded term lookups that must be intersected with the text match, and a
wide range expands into more terms while excluding nothing. Consequence for the
UI: an "All time" option must send **no** `from`/`to` at all, never wide bounds.
A narrow window is worth sending; a wide one is worse than none.

**The two paths are separate, never hybrid.** The FTS path sends one query to
`/api/index/fts_notifications/query` containing everything — tenant term, text
disjunction, keyword filters, and the time window as an FTS date-range clause.
GSI is not involved. The GSI path never touches FTS: it converts the time window
into a **`sort_key` string range** via `ulidFloor`/`ulidCeil`, because the ULID
prefix encodes the timestamp. Measured server-side, that range scan over 500M
documents costs **1.9 ms**.

Three alternatives were measured and all are worse:

| Approach (1-day window + text) | Latency |
|---|---|
| **Pure FTS, datetime range** (current) | **328 ms** |
| FTS `term_range` on `_id` | **fails** — `TooManyClauses [76551 > maxClauseCount]` |
| N1QL `SEARCH()` + GSI `sort_key` range (true hybrid) | 7,912 ms |

The hybrid is 24x slower: the planner drives from the GSI range (~1.37M
documents for one day) and then evaluates `SEARCH()` **per document**, instead of
intersecting two index result sets. The `term_range` failure is more
instructive still — term ranges *enumerate* matching terms into clauses, and at
500M unique ids any real window exceeds `maxClauseCount`. That is precisely why
numeric and datetime fields use prefix-coded precision steps: the encoding makes
a range resolve to a few coarse-plus-fine terms rather than millions of clauses.
The expansion cost noted above is not a flaw in that design, it is the
mechanism, and the string alternative does not merely cost more — it fails.

> **On converting timestamps to epoch integers** (asked during the PoC): it does
> not help and would hurt. FTS `datetime` fields are already numeric internally
> — bleve parses the ISO value once at index time and indexes it with the same
> prefix-coded scheme as a number, so a range query is already numeric. And
> sorting on the numeric field measured 2-4x *slower* than on a keyword string,
> because numeric fields carry multiple precision-step terms whose docvalues
> cost more to retrieve than a keyword's single term.

> **Two diagnoses this supersedes, both wrong.** First: "the index holds 0 bytes
> in RAM." It does not — `num_bytes_used_ram` reports FTS *heap*, not mmap'd
> pages, and Couchbase's `memoryFree` reports `MemAvailable`, which counts
> reclaimable page cache as available. Both were misread as an idle machine.
> Second: "add a third search node." That redistributes 8 partitions over 3
> machines but leaves total parallelism at 8 threads and per-partition work
> unchanged, on nodes already 74% idle. It would have cost money and achieved
> almost nothing. Neither claim survived measuring the actual machine.

**GSI is comfortable and needs nothing.** Measured on the index nodes:

| Index | Items | Resident | Cache hit | Scan latency | Disk |
|---|---|---|---|---|---|
| `idx_feed` | 500,000,026 | **2%** | 99% | 0.164 ms | 32.0 GB |
| `idx_user` | 500,000,026 | 16% | 99% | 0.147 ms | 34.7 GB |
| `idx_retry` | 24,962,764 | 23% | 99% | 0.114 ms | 1.6 GB |

Index nodes hold 70 GB each, run at load average 0.07, and have 34.8 GB of
genuinely free RAM (not merely "available"). Plasma uses ~25 GB of its 96 GB
quota. `EXPLAIN` confirms `IndexScan3` on `idx_feed` — not a sequential scan.

**A 2% resident ratio alongside a 99% cache-hit rate is the design working, not
a warning.** Both queries are keyset scans anchored at the newest end of the
index, so the hot region is tiny and the 500M-entry tail is never read. Resident
ratio is the wrong metric for this access pattern. Sizing the cluster to make
`idx_feed` resident would have bought ~100 GB of RAM to cache data no query
touches.

**Adding index nodes would not help.** Beyond the nodes being idle, the GSI
indexes are **not partitioned** — each lives entirely on one node with a replica
on the other, so a third node would sit unused unless the indexes were
partitioned (`num_partition`) or replicas added. As with FTS, the lever is
partitioning rather than node count; unlike FTS, there is no reason to pull it.

> **Correction to §7.4.** `idx_retry` is described there as holding "thousands"
> of entries. Against live traffic that holds, but the seeded dataset is 5%
> `PENDING` — 24,962,764 documents match `WHERE status='PENDING'`. Still 20x
> smaller than the collection, and the bounded `next_attempt_at` range keeps
> scans to 0.114 ms over 9,548 requests, but the figure as written is wrong by
> four orders of magnitude.

> **Estimate accuracy, for future sizing.** Data tier over-estimated ~2x
> (376.9 GB actual), GSI over-estimated ~2.4x (67 GB primary actual vs 162 GB
> predicted; Plasma compresses ~3x — `idx_feed` is 99.5 GB logical, 32.0 GB
> stored), FTS **under**-estimated 2.7x (269.5 GB actual vs 80–110 GB). The
> compressed B-tree structures came in far smaller than predicted; the inverted
> index came in far larger. Bias future estimates accordingly.

**Sort field: `_id`, not `sort_key`, and definitely not a numeric field.**
Measured at 139M hits, warm:

| Sort strategy | Latency |
|---|---|
| score (no field sort) | 3.4 s |
| **`_id` (document id)** | **5.9 s** |
| `sort_key` (43-char keyword) | 8.3 s |
| `timestamp` (numeric/datetime) | 17.9 s |

Sorting by `_id` is ~29% faster than by the `sort_key` field and produces
identical ordering, because every id is `ntf::{tenant}::{ULID}::{channel}` and
queries are always tenant-scoped, so the shared prefix makes ordering by the
whole id equivalent to ordering by its ULID suffix. `search_after` works with
it, and the sort value returned *is* the id, so the client already holds its
own cursor.

Sorting on the numeric `timestamp` is **2-4x slower** than either string sort.
The usual "numeric comparison beats string comparison" intuition is inverted in
bleve: numeric and datetime fields are indexed with multiple precision-step
terms (which is what makes their range queries fast), so their docvalues cost
more to retrieve than a keyword's single term. Converting timestamps to epoch
integers would not help; it would hurt.

Adopting `_id` sort improved every FTS query by 25-42%: text with a 7-day
window 4,197 -> 2,965 ms, `status=FAILED` 4,359 -> 2,537 ms, text with a 1-day
window 1,356 -> 1,020 ms. A follow-on saving is available but untested: with
nothing sorting on `sort_key`, its docvalues could be dropped from the index,
shrinking the 269.5 GB footprint.

**Mitigation implemented:** an unscoped full-text search is given a default
time window (`DEFAULT_TEXT_WINDOW_MS`, 7 days) and the response reports the
window applied so the UI can show it and let the operator widen it deliberately.
FTS queries also send `ctl.timeout`, since the 10s server default is exceeded by
a genuinely broad search.

> **A defect worth recording.** The first implementation expressed an open-ended
> range with a sentinel `end: 2999-01-01`. FTS rejects that with
> `invalid/unsupported date range` on **every** partition, and the query returns
> zero hits in ~19 ms — indistinguishable from "nothing matched". A benchmark
> reported it as a dramatic speed-up. Open-ended ranges must omit the bound, and
> `NotificationSearch` now throws when all partitions fail rather than returning
> an empty page.

### 10.1b Load test result, and the pipeline topology it forced

**Target met: 1000 events/sec sustained, 0 rejected.** Verified over 60s runs
against the full 500M dataset. Measured ceiling ~2,900 events/sec (~7,000
notifications/sec) at host load 9.6 of 16 — roughly 3x the requirement.

Getting there required replacing the single-process pipeline of §4.1.

**A single pipeline process capped at ~520 events/sec.** Not a Couchbase limit:
the seeder pushed 79,386 ops/sec through the same host. The container sat at
156% CPU (one JS thread plus native SDK threads) while the host ran at load 0.89
across 16 vCPU. Every Couchbase stage appeared 70-139x slower than at idle,
because `metrics.time()` measures wall time and a callback queued behind a
saturated event loop records as slow I/O.

| Stage | Single process | 6 replicas | Idle baseline |
|---|---|---|---|
| policy (KV) | 57.92 ms | **1.02 ms** | 0.82 ms |
| event_insert (KV) | 73.60 ms | **0.86 ms** | 0.53 ms |
| ntf_insert (KV) | 81.79 ms | **0.93 ms** | 0.59 ms |
| tracking_mutateIn (KV) | 65.73 ms | **0.47 ms** | 0.79 ms |
| end_to_end p50 / p95 | 533 ms / 36.9 s | **50 ms / 119 ms** | — |

Replicated latencies match the idle baseline, confirming the inflation was
queueing and never the database.

**Topology.** `pipeline` is replicated (default 6) behind **nginx**; each
replica is self-contained — its own in-process bus and delivery workers — so
round-robining requests distributes whole units of work with no shared queue.
Verified even: per-replica request counts across a run were 17,633 / 17,649 /
17,681 / 17,688 / 17,721 / 17,736, within 0.6%, at 68-77% CPU each.

Two components must **not** be replicated:

- **The retry scheduler.** Every replica polling `WHERE status='PENDING' AND
  next_attempt_at <= now LIMIT n` receives the same rows and delivers each
  notification N times. It runs only in the `pipeline-scheduler` singleton,
  gated by `ENABLE_SCHEDULER`.
- **The load generator.** An in-process generator can only load its own replica
  — which is what capped the first test. It is now `apps/loadgen`, driving the
  API over HTTP through nginx, which is also what a real producer does.

> **Deployment trap: nginx caches upstream DNS at config load.** With
> `upstream { server pipeline:8080; }`, nginx resolves that name **once** at
> startup. `docker compose up -d --build pipeline` gives the replicas new IPs
> and nginx keeps dialling the dead ones — **every request returns 502** until
> nginx is restarted, with no other symptom. Observed exactly that.
>
> The `resolve` parameter that fixes it on an `upstream{}` block is nginx Plus
> only. On open-source nginx the target must go in a **variable**, forcing a
> per-request lookup against `resolver 127.0.0.11` (Docker's embedded DNS, which
> returns every replica and rotates the order — that rotation is what provides
> the spreading). Note `proxy_pass $var` does **not** append the request URI;
> it must be passed explicitly as `$var$request_uri`.
>
> The cost is upstream keep-alive, which a variable-based `proxy_pass` cannot
> use. Measured rather than assumed: throughput was **996 events/sec before and
> 996 after**, with distribution still even across replicas (7,456–7,653 over a
> 45s run). Keep-alive is not worth a deployment that breaks whenever a replica
> is recreated.

> **Metrics caveat introduced by replication.** `/metrics` is per-replica, so its
> throughput figure is ~1/N of the true rate. The authoritative submitted rate
> is the load generator's `achievedRatePerSec`; the dashboard reads that for
> throughput and labels the per-stage table as a single-replica sample.
> An earlier `ratePerSec` bug compounded this — it averaged over the full 60s
> window including pre-run zeros, making a steady 520/sec read as a ramp
> (70 -> 150 -> 239). Both fixed.

### 10.1c Pipeline rewritten in Go — supersedes §10.1b

The replicated Node topology of §10.1b was replaced by a **single Go process**
using gocb and goroutines. The design is unchanged; only the runtime differs.

**Why.** Every operational defect in this PoC came from replication, not from
the business logic: nginx caching upstream DNS (blanket 502s), metrics sharded
across replicas (Reset appeared broken, throughput read 1/N), the scheduler
needing singleton gating so six replicas did not deliver everything six times,
and the load generator needing extraction because in-process it could only load
its own replica. Node forced that replication by executing JavaScript on one
core. Goroutines use all sixteen from one process, so the entire class of
problem disappears rather than being managed.

| | Node | Go |
|---|---|---|
| Containers | 11 | **3** |
| Sustained rate | 996/sec (ceiling ~2,690) | **5,996/sec** at 100% of target |
| Notifications/sec | ~2,370 | **14,234** |
| Load balancer | nginx required | none |
| Scheduler | env-gated singleton | one by construction |
| Load generator | separate service | in-process goroutines |
| Metrics reset | marker doc + per-replica poll | immediate, one registry |

Verified against ground truth: a 30s run at 5,996 events/sec wrote **427,011**
notifications, and the session counters summed to **427,011** — 0.00% drift,
0 tracking failures. Behaviour is identical to the Node implementation: retry
exhausts at exactly `max_attempts` with correctly jittered backoff, flood
suppression is exact at the tenth message, fan-out honours per-channel template
variants. Search is unchanged (GSI feed 563ms end-to-end, FTS 0.9–1.6s).

**Two defects found during the rewrite, both mine, both fixed:**

- *Statistics were written per bump.* One KV round trip per counter increment
  is ~20,000 extra ops/sec at 6,000 events/sec; the bounded queue in front of it
  overflowed and silently dropped **2.8M bumps**, so the dashboard under-reported
  by 3x while the pipeline was in fact writing every document. Counters are now
  aggregated in memory and flushed once a second — 20,000 ops/sec becomes six,
  and drops are impossible.
- *The load generator spawned unbounded goroutines.* At 10,000/sec they
  outpaced completion, gocb's operation queue overflowed, and the failure
  cascaded into **26,742 tracking errors**. A semaphore now bounds in-flight
  submissions, so overload appears as a lower achieved rate and explicit
  rejections rather than as corruption.

**Ceiling.** 6,000 events/sec is clean. At 10,000 the generator saturates
(7,155 achieved, the remainder rejected by the semaphore) — the honest reading
is that the box sustains ~6,000 events/sec, or **six times the requirement**.

> **Next.js `rewrites()` bakes its upstream at build time** under
> `output: 'standalone'`, so changing `INTERNAL_API_URL` at runtime does nothing
> and an image built against an older topology keeps dialling a host that no
> longer exists (`ENOTFOUND nginx`). The API proxy is therefore a runtime route
> handler at `apps/web/src/app/api/[...path]/route.ts`, which resolves the
> target per request.

### 10.1d Tuning to 15,000 events/sec — three bottlenecks, none of them Couchbase

Raising the target rate surfaced `load generator saturated: max in-flight
reached`. That message names a symptom — the generator's semaphore was full
because submissions were not draining — and the causes took three measurements
to find. Each was in the application, not the database.

| Fix | Effect |
|---|---|
| **Shard the per-channel rate counter** | policy stage 276ms -> 15ms at 6k |
| **`kv_pool_size=8`** (gocb defaults to 1) | policy 274ms -> 7.3ms; inserts 0.7ms |
| **`DELIVERY_WORKERS` 256 -> 1024** | ceiling 7,045 -> **14,991 events/sec** |

**1. A hot key.** `rc::{tenant}::ch::{channel}::{minute}` was ONE document per
channel per minute, taking ~14,000 writes/sec across three documents. Couchbase
serialises writes per document. Now sharded over 32 keys, with each shard
compared against `cap/32` — statistically equivalent for a uniform hash, and
one increment rather than N reads.

**2. One KV connection per node.** gocb's default `kv_pool_size` is 1, so all
key-value traffic to each data node rode a single socket. The process sat at
378% of 1600% available CPU — waiting on I/O, not computing.

**3. Delivery workers, sized by Little's Law.** Each worker blocks ~45ms per
provider call, so 256 per channel sustains 256/0.045 ≈ 5,700 deliveries/sec, or
~17,000 across three channels. At 2.37 notifications per event that capped
ingest at ~7,000 events/sec — which is exactly where throughput plateaued
regardless of offered load. 1024 workers lifted it past 60,000.

**Result: 14,991 events/sec sustained, 0% rejected, queue depth 0** — roughly
35,500 notifications/sec and 90,000+ KV operations/sec, at host load 6.4 of 16.
Fifteen times the PoC requirement.

> **A hypothesis that was wrong, recorded because the reasoning looked sound.**
> With the queue backed up at 241,983 and `mock-channels` at 94.6% of one core,
> the obvious read was that the simulated provider was the bottleneck. Scaling
> it to four replicas spread the load evenly and drained the queue to zero — and
> throughput did not improve at all (5,950 vs 6,442). The provider was saturated
> *as a consequence* of the real limits above, not a cause. The replicas were
> kept because they are genuinely needed at 15,000/sec, but they fixed nothing
> on their own.

### 10.1e Latency, per stage — there is deliberately no end-to-end metric

At 3,000 events/sec on the 500M dataset:

| Stage | p50 | Notes |
|---|---|---|
| policy (3 KV counter increments) | 3.32 ms | |
| render (in-process) | ~0 ms | |
| event insert (KV) | 0.59 ms | |
| notification insert (KV) | 0.49 ms | |
| tracking mutateIn (KV) | 0.28 ms | |
| **Couchbase, per notification** | **4.68 ms** | sum of the above |
| **channel call — the MOCK provider** | **44.16 ms** | ~90% of the journey |
| retry pickup (N1QL, partial index) | 17.28 ms | **background**, once per second — not on any notification's path |

**Couchbase does its share in under 5ms.** The simulated external provider is
roughly 90% of what a notification actually experiences. That separation is the
whole reason the pipeline records per-stage histograms: the number a stakeholder
would quote is dominated by a component this PoC deliberately fakes.

**An end-to-end metric was implemented and then removed.** It conflated three
different things in one figure — pipeline work, time queued on the delivery bus,
and retry backoff — and was dominated by the mock provider, so it invited
precisely the wrong reading. It was also actively wrong during backlog recovery:
notifications republished by the scheduler carry their ORIGINAL timestamp, so
one abandoned ten minutes earlier recorded ten minutes of "latency" and dragged
p50 to 7.8 seconds. Splitting recovered work into its own bucket fixed the
arithmetic but not the confusion, so the metric is gone. The per-stage figures
say the same thing unambiguously.

> **Do not sum every row to get the database's share.** `retry_pickup`,
> `search_fts` and `search_hydrate` are background or query work, not part of a
> notification's path; including `retry_pickup` overstates Couchbase's per-
> notification cost by roughly 4x. The dashboard computes the two summary rows
> rather than leaving that arithmetic to the reader, because the first time it
> was done by hand it was done wrong.

### 10.1f Composite GSI replaces FTS for every non-text filter

**This supersedes D10 and §7.1.** The original reasoning was that arbitrary
filter combinations across seven fields would need a combinatorial explosion of
secondary indexes, so FTS should be the search backend. That reasoning missed
one thing, and it turned out to matter enormously.

`idx_multi` on `(tenant_id, sort_key DESC, status, channel, app_name, type,
seen, user_id)` makes the **leading key the sort order**. The planner seeks to
the newest entry, scans backwards evaluating the trailing predicates *inside the
index* (they are all index keys, so no document fetch), and **stops at LIMIT**.
FTS structurally cannot do that: it must collect and rank every match before
returning a page — which is why `status=FAILED` cost 1.4s there, counting 50M
hits to hand back 100.

Measured through the full API on 500M documents:

| Query | FTS (before) | **GSI `idx_multi`** |
|---|---|---|
| `status=FAILED` | 1,400 ms | **9 ms** |
| `channel=email + status=DELIVERED` | ~600 ms | **9 ms** |
| `app_name=portal` | ~700 ms | **9 ms** |
| `seen=false + channel=push` | ~700 ms | **9 ms** |
| `type=otp_login` | ~700 ms | **8 ms** |
| explicit date range | — | **9 ms** |
| **message text** | 813 ms | **FTS only — unavoidable** |

**70–150x faster**, and correctness verified: every filter returns only matching
rows, 400 rows across 4 pages were all unique and strictly descending, and the
ULID+channel tiebreaker holds across page boundaries.

**Cost.** `idx_multi` is 48.5 GB (plus replica), taking GSI from ~69 GB to
~118 GB per index node against a 96 GB memory quota — so it is no longer fully
cacheable, and every insert maintains one more index. Both were accepted for a
70x query win.

**FTS keeps exactly one job: matching `message` text.** An inverted index is the
right structure for that and a B-tree is not.

> **Trade-off this introduces.** FTS returns `total_hits` for free; a pre-sorted
> GSI scan stops at 100 rows and never counts the rest. An exact total would need
> a `COUNT(*)` costing roughly 100x the query, so it is not queried — the UI says
> "count not queried" rather than implying the filter matched nothing.

> **`idx_feed` and `idx_user` were dropped** — see §7.4 for the measured
> before/after. One earlier claim here was wrong and is worth recording: the
> filtered shapes were *never* being served by `idx_feed`. `EXPLAIN` shows the
> planner already routing every keyword filter to `idx_multi`; `idx_feed`'s ~2,000
> requests were the dashboard's 2-second poll of the unfiltered feed and nothing
> else. So dropping it bought disk and write-path savings, not the 74–86% latency
> win a hinted comparison appeared to promise. Always read `EXPLAIN` before
> attributing a hinted measurement to real traffic.

> **One shape is intrinsically slow, and no index fixes it.** `status='PENDING'`
> with no time window takes **6.1 s**, because the newest `PENDING` document is
> ~26 h older than the newest document overall — every recent notification
> resolved to DELIVERED or FAILED. A newest-first scan must walk **14.3M** index
> entries before the first match. That is a property of the data, not the index:
> `idx_multi` filters those entries inside the index, whereas `idx_feed` would
> have fetched all 14.3M from KV to test `status`. Options if it matters: a
> partial index `WHERE status='PENDING'`, or having the UI default a time window
> when `status=PENDING` is selected alone.

### 10.2 Known infrastructure risks

1. **The data path is on the root filesystem** (`/opt/couchbase/var/lib/couchbase/data`
   on `/`). Capacity is adequate (~40% at steady state) but a runaway seed takes
   the OS down rather than filling a data volume. A dedicated EBS volume mounted
   at the data path is recommended before seeding.
2. **Default credentials on a public address.** `Administrator` / `password` on
   a public EC2 IP should be changed before there is 500M documents of work
   behind it, and the security group restricted to specific `/32` sources.
3. **FTS build time.** Indexing 500M documents is a multi-hour job. Plan it as
   an overnight step.
4. **Sequential-scan fallback is the sharpest trap in this deployment.**
   Couchbase 8.0 answers an unindexed N1QL query by scanning the collection in
   KV rather than refusing it. Verified on this cluster: `EXPLAIN` on a
   `timestamp` predicate reported `"index": "#sequentialscan"`,
   `"using": "sequentialscan"`. At 500 documents that is instant; at 500M it is
   a full scan of roughly 375 GB. It is more dangerous than the old
   "no index available" error because a query that looks healthy in testing
   degrades catastrophically on the real dataset instead of failing loudly.
   Mitigations: build all GSI indexes before the full dataset is queried; keep
   the application on FTS + KV with the retry scheduler's covered query as the
   only N1QL in the hot path; and treat ad-hoc `COUNT(*)` or ungrounded
   predicates against `notifications` as an operational hazard. No exposed
   cluster setting to disable the fallback was found at
   `/settings/querySettings` or the query service admin endpoint.

### 10.3 Provisioning

`infra/setup-cluster.sh` is idempotent and applies everything above. It
auto-detects whether the query and search ports are directly reachable and
falls back to the cluster manager's UI proxy paths (`/_p/query/…`, `/_p/fts/…`)
when `CB_HOST` is a data-only node. It refuses to proceed if a bucket exists
with a non-Magma backend, since that cannot be fixed in place.

---

## 11. Testing

**Unit** — pure functions, no Couchbase: backoff computation, dedup and rate
window arithmetic, template substitution (including missing-parameter errors),
key builders, cursor encode/decode round-trips, ULID floor/ceil for time
windows, failure classification.

**Integration** — against a real Couchbase on a small dataset:

- Full pipeline: event → fan-out → delivery → tracking.
- Retry to exhaustion produces `FAILED` with `trials == max_attempts`.
- A permanent `4xx` produces `FAILED` with `trials == 1`.
- Flood suppression blocks the `n+1`th and writes `suppressed` on the event.
- Replaying an `event_id` creates no second notification.
- Keyset pagination is correct across a boundary where many documents share the
  same `timestamp` — the case offset pagination and naive sorting both break.
- `seen` is never modified by any pipeline component.

**Performance** — the PoC's own load test.

---

## 12. Open items

| Item | Owner | Notes |
|---|---|---|
| Dedicated EBS volume for the data path | infra | §10.2 |
| Rotate cluster credentials, tighten security group | infra | §10.2 |
| Reconcile 100 vs 1,000 in the requirements document | requirements owner | §1.4 |
| Choose the 6–8 seed template types | design | §9.2 |
| Deferred spec: Sync Gateway, Couchbase Lite, IDM | — | §1.3 |
