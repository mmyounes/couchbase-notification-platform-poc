'use client'
import clsx from 'clsx'
import type { Status } from '@ncgr/shared'

export function Panel ({ title, right, children, className }: {
  title?: string; right?: React.ReactNode; children: React.ReactNode; className?: string
}) {
  return (
    <section className={clsx('panel', className)}>
      {title && (
        <div className="panel-hd flex items-center justify-between gap-3">
          <span>{title}</span>
          {right}
        </div>
      )}
      {children}
    </section>
  )
}

const STATUS_STYLE: Record<Status, string> = {
  DELIVERED: 'bg-ok/15 text-ok border-ok/30',
  FAILED: 'bg-danger/15 text-danger border-danger/30',
  PENDING: 'bg-pending/15 text-pending border-pending/30',
}

export function StatusBadge ({ status, trials }: { status: Status; trials?: number }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={clsx('px-1.5 py-0.5 rounded border text-[11px] font-medium', STATUS_STYLE[status])}>
        {status}
      </span>
      {/* "Retried" is not a status - it is trials > 0 (spec D4). */}
      {typeof trials === 'number' && trials > 1 && (
        <span className="text-[11px] text-warn" title={`${trials} delivery attempts`}>
          ×{trials}
        </span>
      )}
    </span>
  )
}

/**
 * Hover/focus explanation for a metric.
 *
 * These numbers are genuinely easy to misread - the tiles mix units (events vs
 * notifications vs attempts) and mix kinds (cumulative totals vs a live gauge).
 * Putting that on the tile itself beats a footnote nobody reads next to the
 * number they are actually looking at.
 */
export function InfoTip ({ text }: { text: string }) {
  return (
    <span className="relative inline-flex group/tip align-middle">
      {/* normal-case is required: the tile label sets `uppercase` and
          text-transform inherits, so a lowercase glyph renders as "I". */}
      <span
        tabIndex={0}
        role="button"
        aria-label={text}
        className="grid h-3.5 w-3.5 place-items-center rounded-full border border-muted/50
                   text-[9px] font-bold leading-none text-muted cursor-help normal-case
                   hover:border-accent hover:text-accent focus:outline-none
                   focus:border-accent focus:text-accent"
      >
        i
      </span>
      {/* focus-within as well as hover, so it is reachable by keyboard */}
      <span
        role="tooltip"
        className="pointer-events-none invisible opacity-0 group-hover/tip:visible
                   group-hover/tip:opacity-100 group-focus-within/tip:visible
                   group-focus-within/tip:opacity-100 transition-opacity
                   absolute bottom-full left-1/2 z-50 mb-2 w-72 -translate-x-1/2
                   rounded-md border border-border bg-bg px-3 py-2 text-[11px]
                   font-normal normal-case leading-relaxed tracking-normal
                   text-text shadow-xl"
      >
        {text}
      </span>
    </span>
  )
}

export function Stat ({ label, value, tone, info }: {
  label: string; value: number | string
  tone?: 'ok' | 'danger' | 'warn' | 'muted'
  info?: string
}) {
  const color = tone === 'ok' ? 'text-ok' : tone === 'danger' ? 'text-danger'
    : tone === 'warn' ? 'text-warn' : 'text-text'
  return (
    <div className="panel px-4 py-3">
      <div className="text-[11px] uppercase tracking-wide text-muted flex items-center gap-1.5">
        <span>{label}</span>
        {info && <InfoTip text={info} />}
      </div>
      <div className={clsx('text-2xl font-semibold tabular-nums mt-0.5', color)}>
        {typeof value === 'number' ? value.toLocaleString() : value}
      </div>
    </div>
  )
}

export function Spark ({ data, height = 40 }: { data: number[]; height?: number }) {
  if (!data.length) return <div style={{ height }} />
  const max = Math.max(...data, 1)
  return (
    <div className="flex items-end gap-px" style={{ height }}>
      {data.map((v, i) => (
        <div key={i} className="flex-1 bg-accent/70 rounded-sm min-w-[2px]"
          style={{ height: `${Math.max(2, (v / max) * height)}px` }}
          title={`${v}/s`} />
      ))}
    </div>
  )
}

export function ErrorNote ({ error }: { error: unknown }) {
  if (!error) return null
  const msg = error instanceof Error ? error.message : String(error)
  return (
    <div className="panel border-danger/40 bg-danger/10 px-4 py-2.5 text-sm text-danger">
      {msg}
    </div>
  )
}

/** microseconds -> human */
export function us (v: number | undefined): string {
  if (v === undefined) return '—'
  if (v < 1000) return `${Math.round(v)}µs`
  if (v < 1_000_000) return `${(v / 1000).toFixed(1)}ms`
  return `${(v / 1_000_000).toFixed(2)}s`
}

export const shortTime = (iso: string) =>
  iso ? iso.slice(11, 23).replace('Z', '') : '—'
export const shortDate = (iso: string) => (iso ? iso.slice(0, 16).replace('T', ' ') : '—')
