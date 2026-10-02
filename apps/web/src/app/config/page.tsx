'use client'
import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import type { PolicyDoc } from '@ncgr/shared'
import * as api from '@/lib/api'
import { Panel, ErrorNote } from '@/components/ui'
import clsx from 'clsx'

export default function ConfigPage () {
  const [tab, setTab] = useState<'policy' | 'templates'>('policy')
  return (
    <div className="space-y-4">
      <div className="flex gap-1">
        {(['policy', 'templates'] as const).map(t => (
          <button key={t} onClick={() => setTab(t)}
            className={clsx('btn', tab === t && 'bg-accent/15 border-accent text-accent')}>
            {t === 'policy' ? 'Policy' : 'Templates'}
          </button>
        ))}
      </div>
      {tab === 'policy' ? <PolicyEditor /> : <TemplateList />}
    </div>
  )
}

function PolicyEditor () {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['policy'], queryFn: api.getPolicy })
  const [draft, setDraft] = useState<PolicyDoc | null>(null)
  useEffect(() => { if (q.data && !draft) setDraft(structuredClone(q.data)) }, [q.data, draft])

  const save = useMutation({
    mutationFn: (p: PolicyDoc) => api.savePolicy(p),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['policy'] }),
  })

  if (!draft) return <Panel><div className="p-4 text-muted text-sm">Loading policy…</div></Panel>

  const num = (path: (d: PolicyDoc) => number, set: (d: PolicyDoc, v: number) => void,
               label: string, hint?: string) => (
    <div>
      <label className="lbl">{label}</label>
      <input className="field" type="number" value={path(draft)}
        onChange={e => setDraft(d => { const c = structuredClone(d!); set(c, Number(e.target.value)); return c })} />
      {hint && <p className="text-[11px] text-muted mt-1">{hint}</p>}
    </div>
  )

  return (
    <div className="space-y-4">
      <ErrorNote error={q.error ?? save.error} />
      <Panel title="Retry policy" right={<span className="text-xs text-muted">policy::global</span>}>
        <div className="p-4 grid grid-cols-2 lg:grid-cols-4 gap-4">
          {num(d => d.retry.max_attempts, (d, v) => { d.retry.max_attempts = v },
            'Max attempts', 'Exhausting this sets FAILED and surfaces "delivery was not successful".')}
          {num(d => d.retry.backoff.initial_ms, (d, v) => { d.retry.backoff.initial_ms = v }, 'Initial backoff (ms)')}
          {num(d => d.retry.backoff.multiplier, (d, v) => { d.retry.backoff.multiplier = v }, 'Multiplier')}
          {num(d => d.retry.backoff.max_ms, (d, v) => { d.retry.backoff.max_ms = v }, 'Max backoff (ms)')}
        </div>
      </Panel>

      <Panel title="Throttle policy">
        <div className="p-4 grid grid-cols-1 lg:grid-cols-3 gap-4">
          {num(d => d.throttle.max_identical_per_user_per_minute,
            (d, v) => { d.throttle.max_identical_per_user_per_minute = v },
            'Identical per user / minute',
            'Same user + event type + channel. Keyed on the receiver, never on message content — ' +
            'a rotating temp password would defeat content hashing.')}
          {num(d => d.throttle.max_per_user_per_hour,
            (d, v) => { d.throttle.max_per_user_per_hour = v }, 'Any message per user / hour')}
          {num(d => d.throttle.max_per_channel_per_minute,
            (d, v) => { d.throttle.max_per_channel_per_minute = v }, 'Per channel / minute')}
        </div>
      </Panel>

      <div className="flex items-center gap-3">
        <button className="btn btn-primary" disabled={save.isPending}
          onClick={() => save.mutate(draft)}>
          {save.isPending ? 'Saving…' : 'Save policy'}
        </button>
        <button className="btn" onClick={() => setDraft(structuredClone(q.data!))}>Revert</button>
        <span className="text-xs text-muted">
          Applies at the next cache refresh (~10s) — the hot path never reads the policy document.
        </span>
        {save.isSuccess && <span className="text-xs text-ok">saved</span>}
      </div>
    </div>
  )
}

function TemplateList () {
  const templates = useQuery({ queryKey: ['templates'], queryFn: api.getTemplates })
  const defs = useQuery({ queryKey: ['event-defs'], queryFn: api.getEventDefs })

  return (
    <div className="space-y-4">
      <ErrorNote error={templates.error ?? defs.error} />
      {(templates.data ?? []).map(t => {
        const def = defs.data?.find(d => d.template_id === t.template_id)
        return (
          <Panel key={t.template_id} title={`${t.template_id}  ·  v${t.version}`}
            right={<span className="text-xs text-muted">
              {def ? `${def.channels.join(', ')} · ${def.priority}` : ''}
            </span>}>
            <div className="p-4 space-y-3">
              {Object.entries(t.variants).map(([ch, v]) => v && (
                <div key={ch} className="border border-border rounded p-3">
                  <div className="text-xs uppercase tracking-wide text-accent mb-1.5">{ch}</div>
                  {(v.subject || v.title) && (
                    <div className="text-sm mb-1">
                      <span className="text-muted text-xs mr-2">{v.subject ? 'subject' : 'title'}</span>
                      {v.subject ?? v.title}
                    </div>
                  )}
                  <div className="text-sm text-muted">{v.body}</div>
                </div>
              ))}
              <div className="text-xs text-muted">
                parameters: <span className="font-mono">{t.params.join(', ')}</span>
              </div>
            </div>
          </Panel>
        )
      })}
      <p className="text-xs text-muted">
        Note the push variant of <span className="font-mono">reset_password</span> deliberately omits
        the password — push bodies render on lock screens. That asymmetry is the argument for
        per-channel templates rather than one body reused three ways.
      </p>
    </div>
  )
}
