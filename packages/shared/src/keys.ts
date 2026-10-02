// Document key construction. Defined once so that no service can invent a
// different key shape. Mirrors spec section 5.

import type { Channel } from './types.js'

export const keys = {
  tenant: (t: string) => `tnt::${t}`,
  eventDef: (t: string, eventType: string) => `evd::${t}::${eventType}`,
  template: (t: string, templateId: string) => `tpl::${t}::${templateId}`,
  policy: () => 'policy::global',

  event: (t: string, eventId: string) => `evt::${t}::${eventId}`,

  /**
   * Deterministic, so the event -> notification hop never needs a query and a
   * replayed event cannot create a second notification.
   */
  notification: (t: string, eventId: string, channel: Channel) =>
    `ntf::${t}::${eventId}::${channel}`,

  device: (t: string, userId: string, deviceId: string) =>
    `dev::${t}::${userId}::${deviceId}`,

  // --- policy-state counters (spec section 5.6) ------------------------------
  // Deduplication keys on tenant + user + event type + channel, NEVER on
  // message content (spec D6). Content hashing fails outright here: every
  // password reset carries a different temp_password, so a content hash differs
  // every time and flood control would never fire.
  dedup: (t: string, userId: string, eventType: string, channel: Channel, minute: string) =>
    `dd::${t}::${userId}::${eventType}::${channel}::${minute}`,

  rateUserHour: (t: string, userId: string, hour: string) => `rc::${t}::${userId}::${hour}`,

  rateChannelMinute: (t: string, channel: Channel, minute: string) =>
    `rc::${t}::ch::${channel}::${minute}`,

  stat: (t: string, name: string) => `stat::${t}::${name}`,
}

/** Sync Gateway channel name. Present on documents from day one so the deferred
 *  mobile work never requires migrating 500M documents. */
export const syncChannel = (t: string, userId: string) => `tenant::${t}::user::${userId}`

/** `sort_key` - the single field providing sort order, cursor and tiebreaker. */
export const sortKey = (eventId: string, channel: Channel) => `${eventId}::${channel}`

// --- time-bucket helpers ----------------------------------------------------

const p2 = (n: number) => String(n).padStart(2, '0')

/** yyyymmddhhmm, UTC. */
export function minuteBucket (d: Date = new Date()): string {
  return `${d.getUTCFullYear()}${p2(d.getUTCMonth() + 1)}${p2(d.getUTCDate())}` +
         `${p2(d.getUTCHours())}${p2(d.getUTCMinutes())}`
}

/** yyyymmddhh, UTC. */
export function hourBucket (d: Date = new Date()): string {
  return `${d.getUTCFullYear()}${p2(d.getUTCMonth() + 1)}${p2(d.getUTCDate())}${p2(d.getUTCHours())}`
}

/** TTLs in seconds - roughly 2x the window, so a counter always outlives its bucket. */
export const TTL = { minute: 120, hour: 7200 } as const
