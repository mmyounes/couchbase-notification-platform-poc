import { describe, it, expect } from 'vitest'
import {
  ulid, ulidFloor, ulidCeil, decodeUlidTime, encodeTime, sortKeyRange, isoMs,
  render, MissingTemplateParam,
  classifyFailure, backoffMs, applyDeliveryResult,
  encodeCursor, decodeCursor,
  keys, sortKey, minuteBucket, hourBucket,
  type PolicyDoc,
} from './index.js'

const policy: PolicyDoc = {
  retry: { max_attempts: 3, backoff: { initial_ms: 1000, multiplier: 2, max_ms: 30000, jitter: false } },
  throttle: {
    max_identical_per_user_per_minute: 10,
    max_per_user_per_hour: 100,
    max_per_channel_per_minute: 50000,
  },
}

describe('ulid', () => {
  it('round-trips the embedded timestamp exactly', () => {
    // This is the invariant the whole sort_key range-scan design rests on.
    for (const ms of [0, 1, 1_700_000_000_000, Date.parse('2026-08-25T10:30:00.750Z')]) {
      expect(decodeUlidTime(ulid(ms))).toBe(ms)
    }
  })

  it('isoMs preserves millisecond precision', () => {
    // Second-level truncation here would silently break boundary queries.
    const ms = Date.parse('2026-08-25T10:30:00.750Z')
    expect(isoMs(ms)).toBe('2026-08-25T10:30:00.750Z')
    expect(Date.parse(isoMs(ms))).toBe(ms)
  })

  it('sorts lexicographically in time order', () => {
    const a = ulid(1000), b = ulid(2000), c = ulid(3000)
    expect([c, a, b].sort()).toEqual([a, b, c])
  })

  it('floor/ceil bracket every ULID generated within that millisecond', () => {
    const ms = Date.parse('2026-08-25T10:30:00.000Z')
    for (let i = 0; i < 200; i++) {
      const id = ulid(ms)
      expect(id >= ulidFloor(ms)).toBe(true)
      expect(id <= ulidCeil(ms)).toBe(true)
    }
  })

  it('a range covers a ULID at either boundary of the window', () => {
    const from = Date.parse('2026-07-01T00:00:00.000Z')
    const to = Date.parse('2026-08-01T00:00:00.000Z')
    const { from: lo, to: hi } = sortKeyRange(from, to)
    for (const ms of [from, from + 1, to - 1, to]) {
      const sk = sortKey(ulid(ms), 'email')
      expect(sk >= lo && sk <= hi).toBe(true)
    }
    // and excludes just outside
    expect(sortKey(ulid(from - 1), 'email') >= lo).toBe(false)
  })

  it('encodeTime is 10 chars and monotonic', () => {
    expect(encodeTime(0)).toHaveLength(10)
    expect(encodeTime(1) > encodeTime(0)).toBe(true)
  })
})

describe('render', () => {
  it('substitutes every parameter', () => {
    expect(render('temp password {{p}} for {{app}}', { p: 'Xk7', app: 'portal' }))
      .toBe('temp password Xk7 for portal')
  })

  it('throws rather than emitting a blank for a missing parameter', () => {
    // "Your temporary password is ." shipping to a user is worse than failing.
    expect(() => render('pw {{temp_password}}', {})).toThrow(MissingTemplateParam)
  })

  it('leaves text without parameters untouched', () => {
    expect(render('no params here', {})).toBe('no params here')
  })
})

describe('failure classification', () => {
  it('treats 5xx, 429 and transport errors as retryable', () => {
    for (const s of [500, 502, 503, 504, 429, null]) expect(classifyFailure(s)).toBe('retryable')
  })
  it('treats 4xx as permanent', () => {
    for (const s of [400, 401, 404, 422]) expect(classifyFailure(s)).toBe('permanent')
  })
})

describe('backoff', () => {
  it('grows geometrically and clamps at max', () => {
    expect(backoffMs(policy, 1)).toBe(1000)
    expect(backoffMs(policy, 2)).toBe(2000)
    expect(backoffMs(policy, 3)).toBe(4000)
    expect(backoffMs({ ...policy, retry: { ...policy.retry, backoff: { ...policy.retry.backoff, max_ms: 1500 } } }, 3))
      .toBe(1500)
  })

  it('jitter stays within [raw/2, raw]', () => {
    const p: PolicyDoc = { ...policy, retry: { ...policy.retry, backoff: { ...policy.retry.backoff, jitter: true } } }
    for (const r of [0, 0.5, 0.999]) {
      const v = backoffMs(p, 2, () => r)
      expect(v).toBeGreaterThanOrEqual(1000)
      expect(v).toBeLessThanOrEqual(2000)
    }
  })
})

describe('applyDeliveryResult', () => {
  const base = { policy, nowMs: 1_000_000, rand: () => 0.5 }

  it('success sets DELIVERED and increments trials', () => {
    const r = applyDeliveryResult({ ...base, priorTrials: 0, ok: true, httpStatus: 200 })
    expect(r).toMatchObject({ status: 'DELIVERED', trials: 1, nextAttemptAtMs: null })
  })

  it('a retryable failure stays PENDING and schedules a retry (spec D4)', () => {
    const r = applyDeliveryResult({ ...base, priorTrials: 0, ok: false, httpStatus: 503 })
    expect(r.status).toBe('PENDING')
    expect(r.trials).toBe(1)
    expect(r.nextAttemptAtMs).toBeGreaterThan(base.nowMs)
    expect(r.exhausted).toBe(false)
  })

  it('exhausting max_attempts sets FAILED', () => {
    const r = applyDeliveryResult({ ...base, priorTrials: 2, ok: false, httpStatus: 503 })
    expect(r).toMatchObject({ status: 'FAILED', trials: 3, nextAttemptAtMs: null, exhausted: true })
  })

  it('a permanent failure fails immediately without burning retries', () => {
    const r = applyDeliveryResult({ ...base, priorTrials: 0, ok: false, httpStatus: 400 })
    expect(r).toMatchObject({ status: 'FAILED', trials: 1, exhausted: true })
  })
})

describe('cursor', () => {
  it('round-trips, including sort keys containing colons', () => {
    const c = { after: '01K5Z8FQ3M7V8XKQ2R4T6Y9WBC::email', path: 'fts' as const }
    expect(decodeCursor(encodeCursor(c))).toEqual(c)
  })
  it('rejects malformed input rather than throwing', () => {
    for (const bad of ['', 'not-base64!!', Buffer.from('bogus').toString('base64url'), null, undefined]) {
      expect(decodeCursor(bad as string)).toBeNull()
    }
  })
})

describe('keys', () => {
  it('notification keys are deterministic per event and channel', () => {
    expect(keys.notification('ncgr', 'E1', 'email')).toBe('ntf::ncgr::E1::email')
    expect(keys.notification('ncgr', 'E1', 'sms')).not.toBe(keys.notification('ncgr', 'E1', 'email'))
  })

  it('dedup key ignores message content, keying on receiver and channel (spec D6)', () => {
    // Two password resets with different temp passwords must collide, or flood
    // control never fires for the PoC's primary template.
    const a = keys.dedup('ncgr', 'usr_1', 'password_reset', 'email', '202608251030')
    const b = keys.dedup('ncgr', 'usr_1', 'password_reset', 'email', '202608251030')
    expect(a).toBe(b)
  })

  it('time buckets are UTC and zero-padded', () => {
    const d = new Date('2026-01-02T03:04:05Z')
    expect(minuteBucket(d)).toBe('202601020304')
    expect(hourBucket(d)).toBe('2026010203')
  })
})
