'use client'
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ALL_CHANNELS, STATUSES, type NotificationDoc } from '@ncgr/shared'
import * as api from '@/lib/api'
import { Panel, Stat, StatusBadge, ErrorNote, shortDate } from '@/components/ui'
import clsx from 'clsx'

/**
 * Quick-fill shortcuts. They only POPULATE the from/to fields - those fields are
 * the single source of truth, so what is sent always matches what is displayed.
 *
 * Leaving both empty sends NO bounds, which is deliberate: measured at 500M, a
 * range covering the whole dataset more than doubles FTS query time
 * (2,373 -> 5,378 ms) while returning the identical rows, because a wide range
 * expands into many prefix-coded terms and excludes nothing. A narrow window is
 * worth sending; a wide one is worse than none.
 */
const QUICK = [
  { label: '1h',  ms: 3_600_000 },
  { label: '24h', ms: 86_400_000 },
  { label: '7d',  ms: 7 * 86_400_000 },
  { label: '30d', ms: 30 * 86_400_000 },
] as const

const pad = (n: number) => String(n).padStart(2, '0')

/**
 * <input type="datetime-local"> holds LOCAL wall-clock time with no zone, while
 * every stored timestamp is UTC. These two helpers are the only place that
 * conversion happens; getting it wrong returns quietly wrong rows rather than
 * an error, which is the worst kind of bug for a search screen.
 */
const localToIso = (local: string): string =>
  local ? new Date(local).toISOString() : ''

const isoToLocal = (iso: string): string => {
  const d = new Date(iso)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
         `T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/**
 * The executed query, on demand.
 *
 * The point of the search screen is to show that Couchbase serves these filters
 * quickly at 500M documents - a latency number alone asks the viewer to take
 * that on trust. Showing the statement makes the claim checkable: the literals
 * are inlined server-side so it pastes straight into the query workbench.
 *
 * <details> rather than a state toggle: it is a disclosure, and the browser
 * already handles keyboard, focus and the open/closed marker correctly.
 */
function QueryDisclosure ({ queries }: { queries?: Array<{ label: string; lang: string; text: string }> }) {
  if (!queries?.length) return null
  return (
    <details className="group panel px-4 py-2.5 text-xs">
      <summary className="cursor-pointer list-none text-muted hover:text-accent
                          focus:outline-none focus-visible:text-accent select-none">
        <span className="inline-block transition-transform group-open:rotate-90">▸</span>
        {' '}Show the {queries.length > 1 ? `${queries.length} queries` : 'query'} behind these results
      </summary>
      <div className="mt-3 space-y-3">
        {queries.map(qq => (
          <div key={qq.label}>
            <div className="flex items-center justify-between gap-3 mb-1">
              <span className="text-muted">{qq.label}</span>
              <span className="text-[10px] uppercase tracking-wide text-muted/70
                               border border-border rounded px-1.5 py-0.5">{qq.lang}</span>
            </div>
            <pre className="overflow-x-auto rounded border border-border bg-bg
                            px-3 py-2 font-mono text-[11px] leading-relaxed text-text">
{qq.text}
            </pre>
          </div>
        ))}
      </div>
    </details>
  )
}

export default function NotificationsPage () {
  // Whole-collection totals: three FTS counts over 514M, cached 30s server-side.
  const dataset = useQuery({
    queryKey: ['dataset'], queryFn: api.getDatasetTotals, refetchInterval: 30_000,
  })
  const [text, setText] = useState('')
  const [userId, setUserId] = useState('')
  const [appName, setAppName] = useState('')
  const [channel, setChannel] = useState('')
  const [status, setStatus] = useState('')
  const [seen, setSeen] = useState('')
  // datetime-local values (local wall time); empty means unbounded on that side.
  const [fromLocal, setFromLocal] = useState('')
  const [toLocal, setToLocal] = useState('')
  const [applied, setApplied] = useState<api.SearchFiltersUI>({})
  const [cursorStack, setCursorStack] = useState<string[]>([])
  const [selected, setSelected] = useState<NotificationDoc | null>(null)

  const cursor = cursorStack[cursorStack.length - 1] ?? null
  const filtersActive = Object.keys(applied).length > 0
  // Live only on page 1 with no filters. A result set that mutates under you
  // cannot be paged through, so filtering or paging pins it.
  const live = !filtersActive && cursorStack.length === 0

  const q = useQuery({
    queryKey: ['notifications', applied, cursor],
    queryFn: () => api.search(applied, cursor),
    refetchInterval: live ? 2000 : false,
  })

  function buildFilters (): api.SearchFiltersUI {
    const f: api.SearchFiltersUI = {}
    if (text.trim()) f.text = text.trim()
    if (userId.trim()) f.user_id = userId.trim()
    if (appName.trim()) f.app_name = appName.trim()
    if (channel) f.channel = channel
    if (status) f.status = status
    if (seen) f.seen = seen
    // Each bound is independent - "everything since Monday" with no end is a
    // legitimate query the backend now honours.
    if (fromLocal) f.from = localToIso(fromLocal)
    if (toLocal) f.to = localToIso(toLocal)
    return f
  }

  const rangeInvalid = Boolean(fromLocal && toLocal && new Date(fromLocal) > new Date(toLocal))

  const applyQuick = (ms: number) => {
    const now = new Date()
    setFromLocal(isoToLocal(new Date(now.getTime() - ms).toISOString()))
    setToLocal(isoToLocal(now.toISOString()))
  }

  const runSearch = () => { setApplied(buildFilters()); setCursorStack([]) }
  const clearAll = () => {
    setText(''); setUserId(''); setAppName(''); setChannel(''); setStatus(''); setSeen('')
    setFromLocal(''); setToLocal(''); setApplied({}); setCursorStack([])
  }

  const rows = q.data?.rows ?? []

  return (
    <div className="space-y-4">
      {/* Collection-wide totals sit above the filters deliberately: they are the
          denominator for everything below. A filtered result of 22 rows means
          something different against 514M than against 500. */}
      <div className="flex items-baseline justify-between">
        <h2 className="text-sm font-medium">Whole collection</h2>
        <span className="text-xs text-muted">
          via FTS · refreshed every 30s
          {dataset.data?.asOf ? ` · as of ${new Date(dataset.data.asOf).toLocaleTimeString()}` : ''}
        </span>
      </div>
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-3">
        <Stat label="Total notifications" value={dataset.data?.total ?? '—'}
          info={'Every notification in the collection — the seeded dataset plus everything created by load runs. Refreshed every 30 seconds.'} />
        <Stat label="DELIVERED" value={dataset.data?.delivered ?? '—'} tone="ok"
          info={'Delivered notifications across the whole collection, not just this session.'} />
        <Stat label="FAILED" value={dataset.data?.failed ?? '—'} tone="danger"
          info={'Failed notifications across the whole collection, not just this session.'} />
        <Stat label="PENDING" value={dataset.data?.pending ?? '—'} tone="muted"
          info={'Pending notifications across the whole collection. Most are historical seed data, which the retry scheduler deliberately ignores.'} />
      </div>

      <Panel title="Filters">
        <div className="p-4 grid grid-cols-2 lg:grid-cols-7 gap-3 items-end">
          <div className="col-span-2 lg:col-span-2">
            <label className="lbl">Message contains</label>
            <input className="field" value={text}
              onChange={e => setText(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && runSearch()} />
          </div>
          <div className="col-span-2">
            <label className="lbl">
              From <span className="normal-case text-muted/70">(local time)</span>
            </label>
            <input className="field" type="datetime-local" value={fromLocal}
              onChange={e => setFromLocal(e.target.value)} />
          </div>
          <div className="col-span-2">
            <label className="lbl">
              To <span className="normal-case text-muted/70">(local time)</span>
            </label>
            <input className="field" type="datetime-local" value={toLocal}
              onChange={e => setToLocal(e.target.value)} />
          </div>
          <div>
            <label className="lbl">User</label>
            <input className="field" value={userId} onChange={e => setUserId(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && runSearch()} />
          </div>
          <div>
            <label className="lbl">App</label>
            <input className="field" value={appName} onChange={e => setAppName(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && runSearch()} />
          </div>
          <div>
            <label className="lbl">Channel</label>
            <select className="field" value={channel} onChange={e => setChannel(e.target.value)}>
              <option value="">any</option>
              {ALL_CHANNELS.map(c => <option key={c} value={c}>{c}</option>)}
            </select>
          </div>
          <div>
            <label className="lbl">Status</label>
            <select className="field" value={status} onChange={e => setStatus(e.target.value)}>
              <option value="">any</option>
              {STATUSES.map(s => <option key={s} value={s}>{s}</option>)}
            </select>
          </div>
          <div>
            <label className="lbl">Seen</label>
            <select className="field" value={seen} onChange={e => setSeen(e.target.value)}>
              <option value="">any</option><option value="true">seen</option><option value="false">unseen</option>
            </select>
          </div>
          <div className="col-span-2 lg:col-span-3">
            <label className="lbl">Quick fill</label>
            <div className="flex flex-wrap gap-1.5">
              {/* `w`, not `q`: `q` is the query result in this scope. */}
              {QUICK.map(w => (
                <button key={w.label} className="btn px-2 py-1 text-xs"
                  onClick={() => applyQuick(w.ms)}>last {w.label}</button>
              ))}
              <button className="btn px-2 py-1 text-xs"
                onClick={() => { setFromLocal(''); setToLocal('') }}
                title="Sends no date bounds at all — faster than a range covering everything">
                any time
              </button>
            </div>
          </div>
          <div className="flex gap-2 col-span-2 lg:col-span-1">
            <button className="btn btn-primary flex-1" onClick={runSearch}
              disabled={rangeInvalid}>Search</button>
            <button className="btn" onClick={clearAll}>Clear</button>
          </div>
        </div>
      </Panel>

      {rangeInvalid && (
        <div className="panel border-danger/40 bg-danger/10 px-4 py-2 text-xs text-danger">
          &ldquo;From&rdquo; is after &ldquo;To&rdquo; — no rows can match that range.
        </div>
      )}
      {(fromLocal || toLocal) && !rangeInvalid && (
        <div className="text-xs text-muted">
          Querying UTC{' '}
          <span className="font-mono text-text">
            {fromLocal ? localToIso(fromLocal) : '(unbounded)'}
          </span>{' '}→{' '}
          <span className="font-mono text-text">
            {toLocal ? localToIso(toLocal) : '(unbounded)'}
          </span>
          {' '}· timestamps are stored in UTC; the pickers above are your local time.
        </div>
      )}

      <ErrorNote error={q.error} />

      <div className="flex items-center gap-4 text-xs">
        <span className={clsx('px-2 py-0.5 rounded border',
          live ? 'border-ok/40 text-ok bg-ok/10' : 'border-border text-muted')}>
          {live ? '● Live' : '⏸ Paused'}
        </span>
        <span className="text-muted">
          {q.data?.path === 'fts' ? 'FTS (message text)' : 'GSI keyset'}
          {/* FTS returns total_hits for free; a pre-sorted GSI scan stops at 100
              rows and never counts the rest, and COUNT(*) over 500M on that
              index times out. Server-side latency is the more useful number to
              show in the space either way. */}
          {q.data?.total !== null && q.data?.total !== undefined
            && <> · {q.data.total.toLocaleString()} hits</>}
          {q.data?.tookMs !== undefined && (
            <> · <span className={clsx('font-medium',
              q.data.tookMs < 50 ? 'text-ok' : q.data.tookMs < 1000 ? 'text-warn' : 'text-danger')}>
              {q.data.tookMs < 1000
                ? `${q.data.tookMs.toFixed(1)} ms`
                : `${(q.data.tookMs / 1000).toFixed(2)} s`}
            </span> server-side
              {q.data.path === 'fts' && q.data.searchMs !== undefined && (
                <span className="text-muted/70">
                  {' '}({q.data.searchMs.toFixed(0)} ms match + {(q.data.hydrateMs ?? 0).toFixed(0)} ms fetch)
                </span>
              )}
            </>
          )}
          {' '}· page {cursorStack.length + 1}
        </span>
        {q.data?.appliedWindowFrom && (
          <span className="text-warn">
            narrowed to {shortDate(q.data.appliedWindowFrom)} — pick a wider window to search further back
          </span>
        )}
        {q.data?.incomplete && (
          <span className="text-danger">results incomplete — some index partitions failed</span>
        )}
        {q.isFetching && <span className="text-muted">loading…</span>}
      </div>

      <Panel>
        <table className="w-full">
          <thead>
            <tr className="text-[11px] uppercase tracking-wide text-muted">
              {['time', 'user', 'app', 'type', 'channel', 'status', 'seen', 'message'].map(h => (
                <th key={h} className="text-left px-3 py-2 font-medium">{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map(r => (
              <tr key={r.sort_key} onClick={() => setSelected(r)}
                className="hover:bg-bg/60 cursor-pointer">
                <td className="cell font-mono text-xs text-muted whitespace-nowrap">{shortDate(r.timestamp)}</td>
                <td className="cell font-mono text-xs">{r.user_id}</td>
                <td className="cell text-xs">{r.app_name}</td>
                <td className="cell text-xs">{r.type}</td>
                <td className="cell text-xs">{r.channel}</td>
                <td className="cell"><StatusBadge status={r.status} trials={r.delivery.trials} /></td>
                <td className="cell text-xs">{r.seen ? '✓' : '·'}</td>
                <td className="cell text-xs text-muted max-w-[420px] truncate">{r.message}</td>
              </tr>
            ))}
            {!rows.length && !q.isFetching && (
              <tr><td className="cell text-muted" colSpan={8}>No matching notifications.</td></tr>
            )}
          </tbody>
        </table>
        <div className="flex items-center justify-between px-3 py-2 border-t border-border">
          <span className="text-xs text-muted">{rows.length} rows (max 100 per page)</span>
          <div className="flex gap-2">
            <button className="btn" disabled={!cursorStack.length}
              onClick={() => setCursorStack(s => s.slice(0, -1))}>← Prev</button>
            <button className="btn" disabled={!q.data?.next}
              onClick={() => setCursorStack(s => [...s, q.data!.next!])}>Next →</button>
          </div>
        </div>
      </Panel>

      <QueryDisclosure queries={q.data?.queries} />

      {selected && (
        <div className="fixed inset-0 bg-black/60 z-30 flex justify-end" onClick={() => setSelected(null)}>
          <div className="w-[640px] max-w-full bg-panel border-l border-border h-full overflow-auto"
            onClick={e => e.stopPropagation()}>
            <div className="panel-hd flex items-center justify-between sticky top-0 bg-panel">
              <span>Notification</span>
              <button className="btn px-2 py-0.5" onClick={() => setSelected(null)}>✕</button>
            </div>
            <div className="p-4 space-y-4">
              <div className="grid grid-cols-2 gap-3 text-sm">
                <Field k="status"><StatusBadge status={selected.status} trials={selected.delivery.trials} /></Field>
                <Field k="channel">{selected.channel}</Field>
                <Field k="user">{selected.user_id}</Field>
                <Field k="app">{selected.app_name}</Field>
                <Field k="type">{selected.type}</Field>
                <Field k="template">{selected.template_id} v{selected.template_version}</Field>
                <Field k="timestamp">{selected.timestamp}</Field>
                <Field k="seen (mobile only)">{String(selected.seen)}</Field>
              </div>
              <div>
                <div className="lbl">subject</div>
                <div className="text-sm">{selected.subject || <span className="text-muted">—</span>}</div>
              </div>
              <div>
                <div className="lbl">message</div>
                <div className="text-sm">{selected.message}</div>
              </div>
              <div>
                <div className="lbl">delivery</div>
                <pre className="text-xs bg-bg border border-border rounded p-3 overflow-auto">
{JSON.stringify(selected.delivery, null, 2)}
                </pre>
              </div>
              <div>
                <div className="lbl">sync channels (Sync Gateway)</div>
                <div className="text-xs font-mono text-muted">{selected.sync_channels.join(', ')}</div>
              </div>
              <div>
                <div className="lbl">sort key / cursor</div>
                <div className="text-xs font-mono text-muted break-all">{selected.sort_key}</div>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function Field ({ k, children }: { k: string; children: React.ReactNode }) {
  return <div><div className="lbl">{k}</div><div>{children}</div></div>
}
