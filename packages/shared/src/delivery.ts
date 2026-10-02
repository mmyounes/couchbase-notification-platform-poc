// Retry scheduling and failure classification. Pure functions - unit-testable
// with no Couchbase and no clock. Mirrors spec section 6.4.

import type { PolicyDoc } from './types.js'

export type FailureClass = 'retryable' | 'permanent'

/**
 * Retrying a malformed request three times is a bug, not resilience.
 * 5xx / timeouts / transport errors are transient; 4xx means the request itself
 * is wrong and will fail identically on every attempt.
 */
export function classifyFailure (status: number | null): FailureClass {
  if (status === null) return 'retryable'          // timeout, connection reset
  if (status >= 500) return 'retryable'
  if (status === 429) return 'retryable'           // rate-limited upstream
  if (status >= 400) return 'permanent'
  return 'retryable'
}

/**
 * Delay before attempt number `trials + 1`, given `trials` already made.
 * With the default policy: ~1s, ~2s, ~4s.
 */
export function backoffMs (policy: PolicyDoc, trials: number, rand: () => number = Math.random): number {
  const { initial_ms, multiplier, max_ms, jitter } = policy.retry.backoff
  const raw = Math.min(initial_ms * Math.pow(multiplier, Math.max(trials - 1, 0)), max_ms)
  if (!jitter) return Math.round(raw)
  // Full jitter over [raw/2, raw] - spreads a synchronised burst of failures
  // instead of retrying them all in the same millisecond.
  return Math.round(raw / 2 + rand() * (raw / 2))
}

export interface TrackingOutcome {
  status: 'PENDING' | 'DELIVERED' | 'FAILED'
  trials: number
  nextAttemptAtMs: number | null
  lastError: string | null
  exhausted: boolean
}

/**
 * The Tracking Manager's decision, as a pure function of the delivery result.
 * Note a failed attempt leaves status PENDING (spec D4) - only exhausting
 * max_attempts sets FAILED.
 */
export function applyDeliveryResult (opts: {
  policy: PolicyDoc
  priorTrials: number
  ok: boolean
  httpStatus: number | null
  error?: string
  nowMs: number
  rand?: () => number
}): TrackingOutcome {
  const { policy, priorTrials, ok, httpStatus, error, nowMs, rand } = opts
  const trials = priorTrials + 1

  if (ok) {
    return { status: 'DELIVERED', trials, nextAttemptAtMs: null, lastError: null, exhausted: false }
  }

  const cls = classifyFailure(httpStatus)
  const lastError = error ?? (httpStatus === null ? 'transport error' : `HTTP ${httpStatus}`)

  if (cls === 'permanent') {
    return { status: 'FAILED', trials, nextAttemptAtMs: null, lastError, exhausted: true }
  }
  if (trials >= policy.retry.max_attempts) {
    return { status: 'FAILED', trials, nextAttemptAtMs: null, lastError, exhausted: true }
  }
  return {
    status: 'PENDING',
    trials,
    nextAttemptAtMs: nowMs + backoffMs(policy, trials, rand),
    lastError,
    exhausted: false,
  }
}
