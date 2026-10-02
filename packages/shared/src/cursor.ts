// Pagination cursor. Both query paths (GSI keyset and FTS search_after) sort on
// `sort_key`, so one opaque cursor format serves both and the UI has a single
// pagination component (spec section 7.2).

export interface Cursor {
  /** The last sort_key of the previous page. */
  after: string
  /** Which backend produced it, so we never feed a cursor to the wrong path. */
  path: 'gsi' | 'fts'
}

// btoa/atob rather than Buffer: this package is imported by the browser bundle
// as well as the services, so it must stay isomorphic.

function toBase64Url (s: string): string {
  const bytes = new TextEncoder().encode(s)
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function fromBase64Url (s: string): string {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/')
  const bin = atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4))
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return new TextDecoder().decode(bytes)
}

export function encodeCursor (c: Cursor): string {
  return toBase64Url(`${c.path}:${c.after}`)
}

export function decodeCursor (s: string | undefined | null): Cursor | null {
  if (!s) return null
  let raw: string
  try {
    raw = fromBase64Url(s)
  } catch {
    return null
  }
  const i = raw.indexOf(':')
  if (i < 0) return null
  const path = raw.slice(0, i)
  const after = raw.slice(i + 1)
  if ((path !== 'gsi' && path !== 'fts') || !after) return null
  return { path, after }
}

/** Hard cap on results per page (spec section 8.3). */
export const PAGE_SIZE = 100
