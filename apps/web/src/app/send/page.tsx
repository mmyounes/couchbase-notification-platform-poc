'use client'
import { useEffect, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ALL_CHANNELS, CBLITE_CHANNEL, type Channel, type TraceEntry } from '@ncgr/shared'
import * as api from '@/lib/api'
import { Panel, ErrorNote, shortTime } from '@/components/ui'
import clsx from 'clsx'

const KIND_STYLE: Record<string, string> = {
  accepted: 'text-muted',
  dedup_ok: 'text-muted',
  rate_ok: 'text-muted',
  event_persisted: 'text-muted',
  notification_created: 'text-text',
  attempt: 'text-text',
  delivered: 'text-ok',
  failed: 'text-danger',
  scheduled_retry: 'text-warn',
  suppressed: 'text-warn',
}

export default function SendPage () {
  const [userId, setUserId] = useState('usr_000042')
  const [appName, setAppName] = useState('ncgrdemo')
  const [type, setType] = useState('reset_password')
  const [repeat, setRepeat] = useState(1)
  const [forceFail, setForceFail] = useState<Partial<Record<Channel, boolean>>>({})
  const [selected, setSelected] = useState<Partial<Record<Channel, boolean>>>({})
  const [eventIds, setEventIds] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [suppressedNote, setSuppressedNote] = useState<string | null>(null)

  const defs = useQuery({ queryKey: ['event-defs'], queryFn: api.getEventDefs })
  const templates = useQuery({ queryKey: ['templates'], queryFn: api.getTemplates })

  const activeId = eventIds[0]
  // Poll the trace while anything is still in flight. Retries take seconds, so
  // the terminal state arrives well after the HTTP response.
  const trace = useQuery({
    queryKey: ['trace', activeId],
    queryFn: () => api.getTrace(activeId!),
    enabled: Boolean(activeId),
    refetchInterval: 1000,
  })

  const def = defs.data?.find(d => d.event_type === type)
  const tpl = templates.data?.find(t => t.template_id === (def?.template_id ?? type))

  /**
   * Channels the event definition declares. The pipeline intersects any
   * explicit request against these (catalog.go ResolveChannels), so asking for
   * a channel outside the definition is silently dropped rather than sent -
   * and it would have no template variant to render from either. The UI shows
   * all three and disables the rest, so the constraint is visible instead of
   * surprising.
   */
  const declared = useMemo<Channel[]>(() => def?.channels ?? [], [def])

  /**
   * cblite is offered on every event type, whether or not the definition lists
   * it. It is not a provider - the notification is stored and Sync Gateway
   * replicates it to the device - so there is nothing for a definition to
   * enable or a template to author, and the pipeline lets it through on request
   * (catalog.go ResolveChannels).
   */
  const available = useMemo<Channel[]>(
    () => (declared.length ? [...declared, CBLITE_CHANNEL] : []), [declared])

  // Re-arm the selection whenever the event type changes (or the catalog first
  // loads): everything the definition declares is on, nothing is forced to fail.
  useEffect(() => {
    if (!available.length) return
    setSelected(Object.fromEntries(available.map(c => [c, true])))
    setForceFail({})
  }, [type, available])

  const chosen = available.filter(c => selected[c])
  // cblite can never be in here: with no provider to call, there is nothing to
  // fail, so the UI offers no toggle and the pipeline ignores the flag anyway.
  const failing = chosen.filter(c => c !== CBLITE_CHANNEL && forceFail[c])

  /** Supply every parameter the template declares so rendering cannot fail. */
  const paramsFor = (): Record<string, string | number> => {
    const out: Record<string, string | number> = {}
    for (const p of tpl?.params ?? []) out[p] = SAMPLE[p] ?? `${p}-demo`
    return out
  }

  async function send () {
    setBusy(true); setError(null); setSuppressedNote(null)
    try {
      const ids: string[] = []
      let suppressedCount = 0
      for (let i = 0; i < repeat; i++) {
        const r = await api.submitEvent({
          user_id: userId, app_name: appName, type,
          params: paramsFor(),
          channels: chosen,
          // Never claim a channel should fail when it is not being sent - the
          // pipeline would ignore it, but the trace would read as a lie.
          force_fail: Object.fromEntries(failing.map(c => [c, true])),
          traced: true,
          source: 'send_console',
        })
        ids.push(r.event_id)
        if (Object.keys(r.suppressed).length) suppressedCount++
      }
      setEventIds(ids.reverse())
      if (suppressedCount) {
        setSuppressedNote(
          `${suppressedCount} of ${repeat} submissions had at least one channel suppressed by the ` +
          `flood cap. Note this is distinct from delivery failure — nothing was ever sent.`)
      }
    } catch (e) { setError(e) } finally { setBusy(false) }
  }

  return (
    <div className="grid lg:grid-cols-[420px_1fr] gap-5 items-start">
      <div className="space-y-4">
        <Panel title="Compose">
          <div className="p-4 space-y-3">
            <div>
              <label className="lbl">User</label>
              <input className="field" value={userId} onChange={e => setUserId(e.target.value)} />
            </div>
            <div>
              <label className="lbl">App</label>
              <input className="field" value={appName} onChange={e => setAppName(e.target.value)} />
            </div>
            <div>
              <label className="lbl">Event type</label>
              <select className="field" value={type} onChange={e => setType(e.target.value)}>
                {(defs.data ?? []).map(d => (
                  <option key={d.event_type} value={d.event_type}>
                    {d.event_type} → {d.channels.join(', ')}
                  </option>
                ))}
              </select>
            </div>

            {/* One row per channel: whether it fans out at all, and whether its
                delivery is forced to fail. Keeping both on the same row makes
                the dependency obvious - you cannot fail a channel you are not
                sending on. */}
            <div>
              <label className="lbl">Channels</label>
              <div className="space-y-1">
                {ALL_CHANNELS.map(c => {
                  const isCBLite = c === CBLITE_CHANNEL
                  const isDeclared = isCBLite || declared.includes(c)
                  const on = isDeclared && selected[c] === true
                  const fail = on && !isCBLite && forceFail[c] === true
                  return (
                    <div key={c}
                      className={clsx('flex items-center gap-2.5 rounded border px-2.5 py-1.5',
                        isDeclared ? 'border-border' : 'border-border/40')}>
                      <input type="checkbox" checked={on} disabled={!isDeclared}
                        aria-label={`send on ${c}`}
                        onChange={() => setSelected(p => ({ ...p, [c]: !on }))}
                        className="accent-accent disabled:opacity-30" />
                      <span className={clsx('text-sm flex-1',
                        !isDeclared && 'text-muted/50',
                        on ? 'text-text' : 'text-muted')}>{c}</span>
                      {isCBLite ? (
                        <span className="text-[11px] text-muted/70"
                          title="Stored in Couchbase; Sync Gateway replicates it to the device on its next connection. No provider call, so nothing to retry or fail.">
                          stored &amp; synced
                        </span>
                      ) : isDeclared ? (
                        <button
                          disabled={!on}
                          onClick={() => setForceFail(p => ({ ...p, [c]: !fail }))}
                          title={on ? 'Force this channel to fail, across every retry'
                                    : `Select ${c} first`}
                          className={clsx('btn px-2 py-0.5 text-[11px]',
                            fail ? 'bg-danger/15 border-danger text-danger' : 'text-muted',
                            !on && 'opacity-30')}>
                          {fail ? 'force fail' : 'deliver'}
                        </button>
                      ) : (
                        <span className="text-[11px] text-muted/60">
                          not on {type}
                        </span>
                      )}
                    </div>
                  )
                })}
              </div>
              <p className="text-[11px] text-muted mt-1.5">
                {chosen.length === 0
                  ? 'Select at least one channel.'
                  : <>
                      {chosen.length} of {available.length} channel
                      {available.length === 1 ? '' : 's'}
                      {failing.length > 0 && <> · {failing.join(', ')} forced to fail across every retry</>}
                    </>}
              </p>
            </div>
            <div>
              <label className="lbl">Repeat</label>
              <input className="field" type="number" min={1} max={50} value={repeat}
                onChange={e => setRepeat(Number(e.target.value))} />
              <p className="text-[11px] text-muted mt-1">
                Send more than the per-minute cap to trigger flood suppression.
              </p>
            </div>
            <button className="btn btn-primary w-full" onClick={send}
              disabled={busy || chosen.length === 0}>
              {busy ? 'Sending…'
                : chosen.length === 0 ? 'Select a channel'
                : `Send to ${chosen.join(', ')}${repeat > 1 ? ` ×${repeat}` : ''}`}
            </button>
          </div>
        </Panel>

        <ErrorNote error={error} />
        {suppressedNote && (
          <div className="panel border-warn/40 bg-warn/10 px-4 py-2.5 text-sm text-warn">
            {suppressedNote}
          </div>
        )}

        {eventIds.length > 1 && (
          <Panel title={`Recent submissions (${eventIds.length})`}>
            <div className="max-h-52 overflow-auto">
              {eventIds.map(id => (
                <button key={id} onClick={() => setEventIds(p => [id, ...p.filter(x => x !== id)])}
                  className={clsx('block w-full text-left px-3 py-1.5 text-xs font-mono border-b border-border/60 hover:bg-bg',
                    id === activeId && 'bg-accent/10 text-accent')}>
                  {id}
                </button>
              ))}
            </div>
          </Panel>
        )}
      </div>

      <Panel title="Trace" right={activeId
        ? <span className="font-mono text-xs text-muted">{activeId}</span>
        : undefined}>
        {!activeId && (
          <div className="p-8 text-center text-muted text-sm">
            Send a message to see every stage: policy verdicts, persistence, each delivery
            attempt with its backoff, and the terminal outcome.
          </div>
        )}
        {activeId && (
          <div className="divide-y divide-border/60">
            {(trace.data?.entries ?? []).map((e: TraceEntry, i) => (
              <div key={i} className="px-4 py-1.5 flex gap-4 text-sm items-baseline">
                <span className="font-mono text-xs text-muted w-24 shrink-0">{shortTime(e.at)}</span>
                <span className={clsx('w-40 shrink-0 text-xs', KIND_STYLE[e.kind] ?? 'text-text')}>
                  {e.kind}
                </span>
                <span className="w-14 shrink-0 text-xs text-muted">{e.channel ?? ''}</span>
                <span className={clsx('flex-1', KIND_STYLE[e.kind] ?? 'text-text')}>{e.detail}</span>
              </div>
            ))}
            {trace.isError && (
              <div className="px-4 py-3 text-sm text-muted">Trace not available for this event.</div>
            )}
          </div>
        )}
      </Panel>
    </div>
  )
}

const SAMPLE: Record<string, string | number> = {
  app_name: 'ncgrdemo', temp_password: 'Zq4-88Lm', expiry_minutes: 15,
  otp_code: '482915', invoice_no: 'INV-228543', amount: '900.49',
  due_date: '2026-12-25', reference: 'PAY-QLF6JX8ZNK', method: 'Mada card',
  service: 'passport renewal', branch: 'Riyadh Olaya', appointment_time: '09:30',
  ticket: 'TKT-48213', document: 'commercial registration', expiry_date: '2026-11-04',
  days_left: 30, request_no: 'SR-2841903', new_status: 'under review',
  agent: 'Support Desk', activity: 'a sign-in from a new device',
  city: 'Riyadh', occurred_at: '14:22',
}
