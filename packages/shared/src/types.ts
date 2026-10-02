// Document shapes. These are the single source of truth: every service and the
// UI import from here, so the UI cannot drift from what the pipeline writes.
// Mirrors spec section 5.

/**
 * Provider channels - the ones that make an outbound call to a delivery
 * provider. This is what the Bulk Load page and the load generator fan out
 * over, and it deliberately excludes cblite.
 */
export const CHANNELS = ['email', 'sms', 'push'] as const

/**
 * Delivered by storing the notification and letting Sync Gateway replicate it
 * to the device. No provider call, so no retries, no backoff, and no forced
 * failure. Available on every event type; offered only on the Send page.
 */
export const CBLITE_CHANNEL = 'cblite' as const

/** Every channel a notification document may carry. */
export const ALL_CHANNELS = [...CHANNELS, CBLITE_CHANNEL] as const

export type Channel = (typeof ALL_CHANNELS)[number]
export type ProviderChannel = (typeof CHANNELS)[number]

/**
 * Delivery status vocabulary, exactly as the requirements define it (spec D4).
 * A failed attempt leaves status at PENDING and increments trials; only
 * exhausting max_attempts sets FAILED. There is deliberately no RETRYING
 * state - "retried" is expressed as `delivery.trials > 0`.
 */
export const STATUSES = ['PENDING', 'DELIVERED', 'FAILED'] as const
export type Status = (typeof STATUSES)[number]

/** Why a channel was blocked before any delivery was attempted (spec D5). */
export type SuppressionReason =
  | 'identical_cap'      // same user + event type + channel, over the per-minute cap
  | 'receiver_hourly_cap'
  | 'channel_minute_cap'

export interface DeliveryState {
  trials: number
  last_attempt_at: string | null
  next_attempt_at: string | null
  last_error: string | null
  delivered_at: string | null
}

export interface NotificationDoc {
  tenant_id: string
  event_id: string
  user_id: string
  app_name: string
  /** Business event type, e.g. "password_reset". Not a document discriminator. */
  type: string
  channel: Channel
  subject: string
  message: string
  template_id: string
  template_version: number
  /**
   * Millisecond precision, exactly equal to the time embedded in the event_id
   * ULID. This is a correctness requirement: with second-level truncation a
   * sort_key range query silently drops boundary documents (spec section 5.2).
   */
  timestamp: string
  status: Status
  /**
   * User-interaction state, distinct from delivery status. Written ONLY by the
   * mobile app via Sync Gateway - no server-side component may write it.
   */
  seen: boolean
  delivery: DeliveryState
  /** `${event_id}::${channel}` - sort order, pagination cursor and tiebreaker. */
  sort_key: string
  sync_channels: string[]
  /**
   * Test affordances, not production fields. Persisted so that a forced failure
   * survives into RETRIES - otherwise the scheduler rebuilds the delivery
   * command from the document, the retry succeeds, and the exhaustion path to
   * FAILED can never be demonstrated.
   */
  test_force_fail?: boolean
  test_force_permanent?: boolean
}

export interface EventDoc {
  event_id: string
  tenant_id: string
  user_id: string
  app_name: string
  type: string
  template_id: string
  params: Record<string, string | number>
  submitted_at: string
  source: string
  /** What the event definition resolved to. */
  channels_requested: Channel[]
  /** The subset that passed policy. Disjoint from keys of `suppressed`. */
  channels_accepted: Channel[]
  suppressed?: Partial<Record<Channel, SuppressionReason>>
}

export interface TemplateVariant {
  subject?: string
  title?: string
  body: string
}

export interface TemplateDoc {
  template_id: string
  tenant_id: string
  version: number
  variants: Partial<Record<Channel, TemplateVariant>>
  params: string[]
}

export interface EventDefDoc {
  tenant_id: string
  event_type: string
  channels: Channel[]
  template_id: string
  priority: 'critical' | 'high' | 'normal'
}

export interface PolicyDoc {
  retry: {
    max_attempts: number
    backoff: { initial_ms: number; multiplier: number; max_ms: number; jitter: boolean }
  }
  throttle: {
    max_identical_per_user_per_minute: number
    max_per_user_per_hour: number
    max_per_channel_per_minute: number
  }
}

export interface TenantDoc {
  tenant_id: string
  name: string
  channels_enabled: Channel[]
  created_at: string
}

export interface DeviceDoc {
  tenant_id: string
  user_id: string
  device_id: string
  platform: 'ios' | 'android'
  push_token: string
  status: 'active' | 'revoked'
  metadata: Record<string, unknown>
}

// --- API contracts ----------------------------------------------------------

export interface SubmitEventRequest {
  user_id: string
  app_name: string
  type: string
  params?: Record<string, string | number>
  /** Optional override; defaults to the event definition's channels. */
  channels?: Channel[]
  /** Per-channel forced failure, for the single-message console. */
  force_fail?: Partial<Record<Channel, boolean>>
  source?: string
}

export interface SubmitEventResult {
  event_id: string
  channels_accepted: Channel[]
  suppressed: Partial<Record<Channel, SuppressionReason>>
}

/**
 * Live SESSION counters for the dashboard - read from KV, never COUNT(*).
 *
 * These count activity since the last reset, NOT the contents of the
 * collection. Units differ deliberately and the UI must say so:
 *   submitted  - EVENTS accepted
 *   delivered / failed / pending - NOTIFICATIONS (one event fans out to ~2.4)
 *   retried    - delivery ATTEMPTS after the first, not notifications
 *   suppressed - per-CHANNEL throttle rejections
 */
export interface StatCounters {
  submitted: number
  delivered: number
  failed: number
  retried: number
  suppressed: number
  pending: number
  /** When the counters were last zeroed. */
  since: string | null
}

/** Whole-collection totals, from FTS. Changes slowly; poll infrequently. */
export interface DatasetTotals {
  delivered: number
  failed: number
  pending: number
  total: number
  asOf: string
}

export type TraceKind =
  | 'accepted' | 'dedup_ok' | 'rate_ok' | 'suppressed'
  | 'event_persisted' | 'notification_created'
  | 'attempt' | 'delivered' | 'failed' | 'scheduled_retry'

export interface TraceEntry {
  at: string
  kind: TraceKind
  channel?: Channel
  detail: string
}
