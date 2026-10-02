// ULID: unique, lexicographically sortable, time-ordered, with the timestamp
// recoverable from the prefix. That last property is what lets a time-window
// filter become a range scan on sort_key (spec section 7.2).
//
// The Go seeder implements the identical encoding - both must agree or seeded
// and live documents will not sort together.

// This package is imported by the browser bundle as well as the services, so it
// must stay isomorphic - no `node:` imports and no Buffer. Web Crypto is
// available as a global in Node 18+ and in every browser.

const B32 = '0123456789ABCDEFGHJKMNPQRSTVWXYZ' // Crockford
const TIME_LEN = 10
const RAND_LEN = 16

export function encodeTime (ms: number, len: number = TIME_LEN): string {
  if (!Number.isFinite(ms) || ms < 0) throw new RangeError(`bad ulid time: ${ms}`)
  let out = ''
  let v = Math.floor(ms)
  for (let i = len - 1; i >= 0; i--) {
    out = B32[v % 32]! + out
    v = Math.floor(v / 32)
  }
  return out
}

function encodeRandom (len: number = RAND_LEN): string {
  const bytes = new Uint8Array(len)
  globalThis.crypto.getRandomValues(bytes)
  let out = ''
  for (let i = 0; i < len; i++) out += B32[bytes[i]! % 32]!
  return out
}

/** ULID whose embedded timestamp is exactly `ms`. */
export function ulid (ms: number = Date.now()): string {
  return encodeTime(ms) + encodeRandom()
}

/** Lowest ULID possible at `ms` - inclusive lower bound for a range scan. */
export const ulidFloor = (ms: number) => encodeTime(ms) + '0'.repeat(RAND_LEN)

/** Highest ULID possible at `ms` - inclusive upper bound for a range scan. */
export const ulidCeil = (ms: number) => encodeTime(ms) + 'Z'.repeat(RAND_LEN)

/** Recover the millisecond timestamp from a ULID. */
export function decodeUlidTime (id: string): number {
  let ms = 0
  for (let i = 0; i < TIME_LEN; i++) {
    const idx = B32.indexOf(id[i]!)
    if (idx < 0) throw new Error(`invalid ULID character at ${i}: ${id[i]}`)
    ms = ms * 32 + idx
  }
  return ms
}

/**
 * Millisecond-precision ISO timestamp. Notification documents MUST use this
 * rather than second-level truncation, so `timestamp` equals the ULID time
 * exactly - see NotificationDoc.timestamp.
 */
export const isoMs = (ms: number) => new Date(ms).toISOString()

/** Convert a time window into inclusive sort_key range bounds. */
export function sortKeyRange (fromMs: number, toMs: number): { from: string; to: string } {
  return { from: ulidFloor(fromMs), to: ulidCeil(toMs) }
}
