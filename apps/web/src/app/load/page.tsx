'use client'
import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { CHANNELS, type Channel } from '@ncgr/shared'
import * as api from '@/lib/api'
import { Panel, Stat, Spark, ErrorNote, us } from '@/components/ui'
import clsx from 'clsx'

// Stages on a notification's own path. Summing these gives Couchbase's share
// of one notification. retry_pickup / search_* are deliberately excluded: they
// are background or query work, not part of any single notification's journey,
// and including them overstates the database's cost several-fold.
/**
 * Stage catalogue, in the order the pipeline executes them.
 *
 * `op` names the actual read/write performed, because the stage name alone is
 * ambiguous - "policy checks" sent a reader looking in the `policies`
 * collection when the stage in fact increments counters and never reads the
 * policy document at all (it is served from the in-memory catalog cache).
 *
 * `couchbase: false` marks work that is NOT a Couchbase call. Those rows are
 * orange so the eye separates "what the database costs" from "what the mock
 * provider and local CPU cost" - roughly a 30x difference, and the single most
 * misread thing on this page.
 *
 * `perNotification` marks stages on the notification path. The rest
 * (background retry pickup, on-demand search) are shown but excluded from the
 * totals, because they have no per-notification denominator.
 */
interface StageMeta {
  label: string
  op: string
  couchbase: boolean
  perNotification: boolean
}

const STAGES: Array<[string, StageMeta]> = [
  ['policy',            { label: 'counters checks',       op: 'KV incr ×3 per channel',    couchbase: true,  perNotification: true }],
  ['event_insert',      { label: 'event insert',          op: 'KV insert',                 couchbase: true,  perNotification: true }],
  ['render',            { label: 'template render',       op: 'in-memory, no I/O',         couchbase: false, perNotification: true }],
  ['ntf_insert',        { label: 'notification insert',   op: 'KV insert',                 couchbase: true,  perNotification: true }],
  ['channel_call',      { label: 'channel call',          op: 'HTTP POST → mock provider', couchbase: false, perNotification: true }],
  ['tracking_mutatein', { label: 'tracking update',       op: 'KV subdoc mutateIn',        couchbase: true,  perNotification: true }],
  ['retry_pickup',      { label: 'retry pickup',          op: 'N1QL query — per batch',    couchbase: true,  perNotification: false }],
  ['search_gsi',        { label: 'search — filters',      op: 'N1QL query — per search',   couchbase: true,  perNotification: false }],
  ['search_fts',        { label: 'search — message text', op: 'FTS query — per search',    couchbase: true,  perNotification: false }],
  ['search_hydrate',    { label: 'search hydrate',        op: 'KV bulk get — per search',  couchbase: true,  perNotification: false }],
]

export default function BulkLoadPage () {
  const qc = useQueryClient()
  const [rate, setRate] = useState(1000)
  const [duration, setDuration] = useState(60)
  const [pool, setPool] = useState(100_000)
  const [failPct, setFailPct] = useState(10)
  const [channels, setChannels] = useState<Channel[]>([...CHANNELS])

  const stats = useQuery({ queryKey: ['stats'], queryFn: api.getStats, refetchInterval: 1000 })
  const metrics = useQuery({ queryKey: ['metrics'], queryFn: api.getMetrics, refetchInterval: 1000 })
  const load = useQuery({ queryKey: ['load'], queryFn: api.getLoad, refetchInterval: 1000 })
  // Retry work continues in the background after a load run ends, so counters
  // can move with the generator stopped. Surfaced rather than left mysterious.
  const health = useQuery({ queryKey: ['health'], queryFn: api.getHealth, refetchInterval: 2000 })
  const [lastPicked, setLastPicked] = useState<number | null>(null)
  const [retryActive, setRetryActive] = useState(false)
  useEffect(() => {
    const p = health.data?.scheduler.picked
    if (p === undefined) return
    setRetryActive(lastPicked !== null && p > lastPicked)
    setLastPicked(p)
  }, [health.data?.scheduler.picked, lastPicked])

  const start = useMutation({
    mutationFn: () => api.startLoad({
      ratePerSec: rate, durationSec: duration, userPool: pool,
      channels, failureInjection: failPct / 100,
    }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['load'] }),
  })
  const stop = useMutation({
    mutationFn: api.stopLoad,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['load'] }),
  })
  const reset = useMutation({
    mutationFn: async () => { await api.resetStats(); await api.resetMetrics() },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['stats'] })
      qc.invalidateQueries({ queryKey: ['metrics'] })
    },
  })

  const running = load.data?.running ?? false
  const s = stats.data
  const m = metrics.data

  /**
   * Executions of `stage` per notification. Multiplying a stage's latency by
   * this converts a per-execution measurement into that stage's contribution to
   * one notification, which is the only basis on which the stages can be summed.
   * Returns 1 before any traffic, so an empty dashboard shows raw values rather
   * than dividing by zero.
   */
  const scale = (stage: string): number => {
    const notifs = m?.stages.ntf_insert?.count ?? 0
    const count = m?.stages[stage]?.count ?? 0
    return notifs > 0 && count > 0 ? count / notifs : 1
  }

  const toggleChannel = (c: Channel) =>
    setChannels(prev => prev.includes(c) ? prev.filter(x => x !== c) : [...prev, c])

  return (
    <div className="space-y-5">
      <ErrorNote error={stats.error ?? metrics.error ?? load.error ?? start.error ?? stop.error ?? reset.error} />

      <Panel title="Load console" right={
        <span className={clsx('text-xs px-2 py-0.5 rounded border',
          running ? 'border-ok/40 text-ok bg-ok/10' : 'border-border text-muted')}>
          {running ? `running · ${load.data?.elapsedSec}s` : 'idle'}
        </span>
      }>
        <div className="p-4 grid grid-cols-2 lg:grid-cols-6 gap-4 items-end">
          <div>
            <label className="lbl">Rate (events/sec)</label>
            <input className="field" type="number" min={1} value={rate}
              onChange={e => setRate(Number(e.target.value))} disabled={running} />
          </div>
          <div>
            <label className="lbl">Duration (sec, 0 = until stopped)</label>
            <input className="field" type="number" min={0} value={duration}
              onChange={e => setDuration(Number(e.target.value))} disabled={running} />
          </div>
          <div>
            <label className="lbl">User pool</label>
            <input className="field" type="number" min={1} value={pool}
              onChange={e => setPool(Number(e.target.value))} disabled={running} />
          </div>
          <div>
            <label className="lbl">Failure injection %</label>
            <input className="field" type="number" min={0} max={100} value={failPct}
              onChange={e => setFailPct(Number(e.target.value))} disabled={running} />
          </div>
          <div>
            <label className="lbl">Channels</label>
            <div className="flex gap-1.5">
              {CHANNELS.map(c => (
                <button key={c} onClick={() => toggleChannel(c)} disabled={running}
                  className={clsx('btn px-2 py-1 text-xs',
                    channels.includes(c) && 'bg-accent/15 border-accent text-accent')}>
                  {c}
                </button>
              ))}
            </div>
          </div>
          <div className="flex gap-2">
            {running
              ? <button className="btn btn-danger flex-1" onClick={() => stop.mutate()}>■ Stop</button>
              : <button className="btn btn-primary flex-1" onClick={() => start.mutate()}>▶ Start</button>}
            {/* Enabled during a run on purpose: "start measuring from now" is a
                legitimate thing to want mid-run, and a dead button reads as a
                broken one. */}
            <button className="btn" onClick={() => reset.mutate()} disabled={reset.isPending}
              title="Zero the session counters and the latency histograms">
              {reset.isPending ? 'Resetting…' : reset.isSuccess ? 'Reset ✓' : 'Reset'}
            </button>
          </div>
        </div>
        {load.data?.lastError && (
          <div className="px-4 pb-3 text-xs text-danger">{load.data.lastError}</div>
        )}
      </Panel>

      <div className="flex items-baseline justify-between">
        <h2 className="text-sm font-medium">This session</h2>
        <span className="text-xs text-muted">
          since {s?.since ? new Date(s.since).toLocaleString() : 'service start'} · press Reset to zero
        </span>
      </div>
      <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-3">
        {/* Units differ between tiles on purpose - events vs notifications vs
            attempts - and one is a gauge rather than a total. Each tile carries
            its own explanation. */}
        <Stat label="Events submitted" value={s?.submitted ?? 0}
          info={'Events accepted by the pipeline since the last reset. One event fans out to several notifications — one per channel on its event definition.'} />
        <Stat label="In flight now" value={s?.pending ?? 0}
          tone={(s?.pending ?? 0) > 50_000 ? 'warn' : 'muted'}
          info={'Notifications created but not yet delivered or failed. This is a live count, not a total, so Reset leaves it alone. It falls back to zero on its own as the work completes.'} />
        <Stat label="Notifications delivered" value={s?.delivered ?? 0} tone="ok"
          info={'Notifications successfully delivered to a channel. Counted per notification, not per event.'} />
        <Stat label="Notifications failed" value={s?.failed ?? 0} tone="danger"
          info={'Notifications that gave up: either every retry was used, or the provider rejected them permanently. A temporary failure schedules a retry instead.'} />
        <Stat label="Retry attempts" value={s?.retried ?? 0} tone="warn"
          info={'Delivery attempts after the first. A notification that fails three times counts as two retries.'} />
        <Stat label="Channels suppressed" value={s?.suppressed ?? 0} tone="warn"
          info={'Blocked by the throttle policy before any delivery was attempted. Nothing was sent and no notification was created.'} />
      </div>
      {retryActive && (
        <div className="panel border-warn/40 bg-warn/10 px-4 py-2 text-xs text-warn -mt-1">
          The retry scheduler is working through a backlog — {(health.data?.scheduler.picked ?? 0).toLocaleString()}
          {' '}notifications picked up so far. Failed and retried will keep climbing with the load
          generator stopped, because these are retries of work submitted earlier.
        </div>
      )}
      <p className="text-xs text-muted -mt-2">Activity since the last reset.</p>

      <div className="grid lg:grid-cols-2 gap-5">
        <Panel title="Throughput (last 60s)">
          <div className="p-4 space-y-4">
            <div>
              <div className="flex justify-between text-xs text-muted mb-1">
                <span>events/sec submitted</span>
                <span className="tabular-nums text-text text-base font-semibold">
                  {load.data?.achievedRatePerSec ?? 0}
                </span>
              </div>
              <Spark data={m?.throughput.eventsSeries ?? []} />
            </div>
            <div>
              <div className="flex justify-between text-xs text-muted mb-1">
                <span>notifications/sec (fan-out across channels)</span>
                <span className="tabular-nums text-text">{m?.throughput.notificationsPerSec ?? 0}</span>
              </div>
              <Spark data={m?.throughput.notificationsSeries ?? []} />
            </div>
            <div className="flex gap-6 text-xs text-muted pt-1">
              <span>bus depth <span className="text-text tabular-nums">{m?.busDepth ?? 0}</span></span>
              <span>retries picked <span className="text-text tabular-nums">{m?.scheduler.picked ?? 0}</span></span>
              <span>in flight <span className="text-text tabular-nums">{load.data?.inFlight ?? 0}</span></span>
            </div>
          </div>
        </Panel>

        {/*
          * Latency columns show each stage's contribution to ONE notification,
          * not its raw per-execution measurement, so the rows visibly sum to
          * the totals. They do not share a denominator otherwise: `policy` and
          * `event_insert` fire once per EVENT (~2.37 notifications), while
          * `channel_call` and `tracking_mutatein` fire once per delivery
          * ATTEMPT (slightly more than one, because of retries). Stages off the
          * notification path keep their raw values - their `op` states the unit.
          * Hover a row for the measurement behind the scaled figure.
          */}
        <Panel title="Per-stage latency">
          <table className="w-full">
            <thead>
              <tr className="text-[11px] uppercase tracking-wide text-muted">
                <th className="text-left px-3 py-2 font-medium">stage</th>
                <th className="text-right px-3 py-2 font-medium">n</th>
                <th className="text-right px-3 py-2 font-medium">p50</th>
                <th className="text-right px-3 py-2 font-medium">p95</th>
                <th className="text-right px-3 py-2 font-medium">p99</th>
              </tr>
            </thead>
            <tbody>
              {STAGES.filter(([k]) => (m?.stages[k]?.count ?? 0) > 0).map(([k, meta]) => {
                const v = m!.stages[k]!
                const f = meta.perNotification ? scale(k) : 1
                return (
                  <tr key={k} className={clsx(!meta.couchbase && 'text-warn')}
                    title={meta.perNotification && f !== 1
                      ? `measured ${us(v.p50)} per execution, ×${f.toFixed(2)} executions per notification`
                      : `measured ${us(v.p50)}`}>
                    <td className="cell">
                      {meta.label}
                      <span className={clsx('ml-2 text-[10px] font-mono',
                        meta.couchbase ? 'text-muted' : 'text-warn/70')}>{meta.op}</span>
                    </td>
                    <td className="cell text-right tabular-nums text-muted">{v.count.toLocaleString()}</td>
                    <td className="cell text-right tabular-nums">{us(v.p50 * f)}</td>
                    <td className="cell text-right tabular-nums">{us(v.p95 * f)}</td>
                    <td className="cell text-right tabular-nums">{us(v.p99 * f)}</td>
                  </tr>
                )
              })}
              {!Object.values(m?.stages ?? {}).some(v => v.count > 0) && (
                <tr><td className="cell text-muted" colSpan={5}>No traffic yet — start a load run.</td></tr>
              )}
              {(m?.stages.ntf_insert?.count ?? 0) > 0 && (() => {
                const notifs = m!.stages.ntf_insert!.count
                const sum = (want: boolean) => STAGES
                  .filter(([, meta]) => meta.perNotification && meta.couchbase === want)
                  .reduce((n, [k]) => n + (m?.stages[k]?.p50 ?? 0) * scale(k), 0)
                const cb = sum(true)
                const other = sum(false)
                const fanout = (m?.stages.event_insert?.count ?? 0) > 0
                  ? notifs / m!.stages.event_insert!.count : 0
                void notifs
                return (
                  <>
                    <tr className="border-t border-border">
                      <td className="cell font-medium">Couchbase, per notification</td>
                      <td className="cell" />
                      <td className="cell text-right tabular-nums font-medium">{us(cb)}</td>
                      <td className="cell" colSpan={2} />
                    </tr>
                    <tr>
                      <td className="cell text-warn">Not Couchbase, per notification</td>
                      <td className="cell" />
                      <td className="cell text-right tabular-nums text-warn">{us(other)}</td>
                      <td className="cell" colSpan={2} />
                    </tr>
                    <tr>
                      <td className="cell text-[11px] text-muted" colSpan={5}>
                        Latencies are each stage&apos;s contribution to one notification, so the rows
                        above sum to these totals
                        {fanout > 0 && <> — measured fan-out {fanout.toFixed(2)} channels per event</>}.
                        Hover a row for its raw measurement.
                      </td>
                    </tr>
                  </>
                )
              })()}
            </tbody>
          </table>
          <p className="px-3 py-2 text-xs text-muted border-t border-border">
            <span className="text-warn">Orange</span> stages are not Couchbase calls — the channel
            call is the mock provider over HTTP, and the template render is local CPU against the
            cached template. Together they are the large majority of a notification&apos;s journey.
          </p>
        </Panel>
      </div>
    </div>
  )
}
