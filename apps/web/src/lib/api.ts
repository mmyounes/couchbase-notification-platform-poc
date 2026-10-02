// Thin client over the pipeline service. Types come from @ncgr/shared, so the
// UI cannot drift from what the pipeline actually writes.

import type {
  NotificationDoc, PolicyDoc, TemplateDoc, EventDefDoc,
  StatCounters, DatasetTotals, TraceEntry, SubmitEventRequest, SubmitEventResult, Channel,
} from '@ncgr/shared'

// Same-origin by default: Next rewrites /api/* to the pipeline inside the
// compose network (see next.config.mjs). Keeps the API off the public internet
// and removes CORS from the picture entirely.
export const API = process.env.NEXT_PUBLIC_API_URL ?? '/api'

async function req<T> (path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API}${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
    cache: 'no-store',
  })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error((body as { error?: string }).error ?? `HTTP ${res.status}`)
  return body as T
}

// --- dashboard ---------------------------------------------------------------

export interface StageSnapshot {
  count: number; p50: number; p95: number; p99: number; max: number; mean: number
}
export interface MetricsResponse {
  stages: Record<string, StageSnapshot>
  throughput: {
    eventsPerSec: number; notificationsPerSec: number; deliveriesPerSec: number
    eventsSeries: number[]; notificationsSeries: number[]
  }
  busDepth: number
  scheduler: { picked: number; skipped: number; horizonMs: number; lastError: string | null }
}

export interface HealthResponse {
  scheduler: { picked: number; skipped: number; horizonMs: number; lastError: string | null }
  busDepth: number
  goroutines: number
  pendingStatDeltas: number
}

export interface LoadSettings {
  ratePerSec: number; durationSec: number; userPool: number
  channels: Channel[]; failureInjection: number
}
export interface LoadStatus {
  running: boolean; startedAt: string | null; elapsedSec: number
  submitted: number; rejected: number; inFlight: number
  /** Submitted rate as measured by the load generator itself. */
  achievedRatePerSec: number
  settings: LoadSettings | null; lastError: string | null
}

export const getStats = () => req<StatCounters>('/stats')
export const getDatasetTotals = () => req<DatasetTotals>('/stats/dataset')
export const getMetrics = () => req<MetricsResponse>('/metrics')
export const getHealth = () => req<HealthResponse>('/health')
export const getLoad = () => req<LoadStatus>('/load')
export const startLoad = (s: Partial<LoadSettings>) =>
  req<LoadStatus>('/load/start', { method: 'POST', body: JSON.stringify(s) })
export const stopLoad = () => req<LoadStatus>('/load/stop', { method: 'POST' })
export const resetStats = () => req<{ ok: true }>('/stats/reset', { method: 'POST' })
export const resetMetrics = () => req<{ ok: true }>('/metrics/reset', { method: 'POST' })

// --- tenancy -----------------------------------------------------------------

export interface TenantDoc {
  tenant_id: string
  name: string
  channels_enabled: string[]
  created_at: string
}

/** `active` is the tenant the pipeline is bound to at startup. */
export const getTenants = () =>
  req<{ active: string; tenants: TenantDoc[] }>('/tenants')

// --- send console ------------------------------------------------------------

export const submitEvent = (b: SubmitEventRequest & { traced?: boolean }) =>
  req<SubmitEventResult>('/events', { method: 'POST', body: JSON.stringify(b) })

export const getTrace = (eventId: string) =>
  req<{ event_id: string; entries: TraceEntry[] }>(`/trace/${eventId}`)

// --- search ------------------------------------------------------------------

export interface SearchFiltersUI {
  text?: string
  user_id?: string
  app_name?: string
  channel?: string
  status?: string
  type?: string
  seen?: string
  from?: string
  to?: string
}

export interface SearchResponse {
  /** 'none' when the filter combination is impossible and no engine was asked. */
  path: 'gsi' | 'fts' | 'none'
  total: number | null
  rows: NotificationDoc[]
  incomplete: boolean
  /** Server-side time for this query, excluding network and browser. */
  tookMs?: number
  /** FTS path only: the split between matching and document hydration. */
  searchMs?: number
  hydrateMs?: number
  /** Non-null when the server narrowed an unscoped text search. */
  appliedWindowFrom?: string | null
  /** What the server actually executed, in order, for this page of results. */
  queries?: Array<{ label: string; lang: string; text: string }>
  next: string | null
}

export function searchQueryString (f: SearchFiltersUI, cursor?: string | null): string {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(f)) {
    // An empty value must be omitted entirely. In particular an "All time"
    // window must send NO from/to: a range covering the whole dataset more than
    // doubles FTS query time while filtering nothing (measured at 500M).
    if (v !== undefined && v !== null && String(v).trim() !== '') p.set(k, String(v))
  }
  if (cursor) p.set('cursor', cursor)
  const qs = p.toString()
  return qs ? `?${qs}` : ''
}

export const search = (f: SearchFiltersUI, cursor?: string | null) =>
  req<SearchResponse>(`/notifications${searchQueryString(f, cursor)}`)

/**
 * Same endpoint and same filters, but pinned to the Search service even when no
 * message text is supplied - which the default `search` would route to the GSI.
 * Exists for the /notifications-fts comparison page; deliberately a separate
 * function so the dashboard's own call path stays untouched.
 */
export const searchFts = (f: SearchFiltersUI, cursor?: string | null) => {
  const qs = searchQueryString(f, cursor)
  return req<SearchResponse>(`/notifications${qs ? `${qs}&` : '?'}engine=fts`)
}

// --- configuration -----------------------------------------------------------

export const getPolicy = () => req<PolicyDoc>('/policy')
export const savePolicy = (p: PolicyDoc) =>
  req<PolicyDoc>('/policy', { method: 'PUT', body: JSON.stringify(p) })
export const getTemplates = () => req<TemplateDoc[]>('/templates')
export const getEventDefs = () => req<EventDefDoc[]>('/event-defs')
