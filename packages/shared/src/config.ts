// Environment-derived configuration, shared so every service reads the same
// names and defaults.

export interface CouchbaseConfig {
  connectionString: string
  username: string
  password: string
  bucket: string
  scope: string
}

export interface AppConfig {
  couchbase: CouchbaseConfig
  tenantId: string
  mockChannelsUrl: string
  /** How often the in-memory policy cache is refreshed from Couchbase. */
  policyRefreshMs: number
  /** Retry scheduler tick. */
  retryTickMs: number
  /** Max notifications claimed per retry tick. */
  retryBatch: number
  /** Concurrency per delivery-bus topic. */
  deliveryConcurrency: number
  /**
   * Grace period stamped into next_attempt_at when a notification is created.
   * Normal deliveries resolve well before it expires; only a notification whose
   * delivery command was lost is still PENDING when it does, at which point the
   * retry scheduler picks it up. This is what makes reconciliation free.
   */
  staleAfterMs: number
  /**
   * Only notifications due within this window are eligible for retry.
   * Without it, the millions of historical PENDING notifications in a 500M
   * dataset all look due and the scheduler never stops re-delivering them.
   */
  retryHorizonMs: number
  port: number
  logLevel: string
}

const int = (v: string | undefined, d: number) => {
  const n = v === undefined ? NaN : Number(v)
  return Number.isFinite(n) ? n : d
}

export function loadConfig (env: NodeJS.ProcessEnv = process.env): AppConfig {
  return {
    couchbase: {
      connectionString: env.CB_CONNECTION ?? `couchbase://${env.CB_HOST ?? '127.0.0.1'}`,
      username: env.CB_USER ?? 'Administrator',
      password: env.CB_PASS ?? 'password',
      bucket: env.CB_BUCKET ?? 'ncgr',
      scope: env.CB_SCOPE ?? 'platform',
    },
    tenantId: env.TENANT_ID ?? 'ncgr',
    mockChannelsUrl: env.MOCK_CHANNELS_URL ?? 'http://mock-channels:8081',
    policyRefreshMs: int(env.POLICY_REFRESH_MS, 10_000),
    retryTickMs: int(env.RETRY_TICK_MS, 1_000),
    retryBatch: int(env.RETRY_BATCH, 500),
    deliveryConcurrency: int(env.DELIVERY_CONCURRENCY, 256),
    staleAfterMs: int(env.STALE_AFTER_MS, 60_000),
    retryHorizonMs: int(env.RETRY_HORIZON_MS, 3_600_000),
    port: int(env.PORT, 8080),
    logLevel: env.LOG_LEVEL ?? 'info',
  }
}
