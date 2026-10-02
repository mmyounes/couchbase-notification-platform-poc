// Mock delivery channels: email, SMS, push.
//
// Runs as its own process, deliberately. If these simulated delays shared an
// event loop with the Tracking Manager they would contaminate the very latency
// measurements the PoC exists to produce (spec section 4.1).
//
// Failure model (spec section 6.4):
//   503 -> retryable: the pipeline backs off and increments trials
//   400 -> permanent: straight to FAILED without burning three attempts
// Both paths matter; retrying a malformed request three times is a bug.

import Fastify from 'fastify'
import { CHANNELS, type Channel } from '@ncgr/shared'

const PORT = Number(process.env.PORT ?? 8081)

interface ChannelProfile {
  /** Base latency in ms, before jitter. */
  baseMs: number
  jitterMs: number
}

/** Rough stand-ins for real provider behaviour: SMTP slow, FCM fast. */
const PROFILES: Record<Channel, ChannelProfile> = {
  email: { baseMs: 25, jitterMs: 60 },
  sms:   { baseMs: 40, jitterMs: 90 },
  push:  { baseMs: 8,  jitterMs: 25 },
}

interface Settings {
  /** Probability [0,1] that a request fails when not explicitly forced. */
  failureRate: number
  /** Of those failures, the share that are permanent 4xx rather than 5xx. */
  permanentShare: number
  latencyScale: number
  enabled: boolean
}

const settings: Settings = {
  failureRate: Number(process.env.FAILURE_RATE ?? 0),
  permanentShare: Number(process.env.PERMANENT_SHARE ?? 0.2),
  latencyScale: Number(process.env.LATENCY_SCALE ?? 1),
  enabled: true,
}

const stats: Record<string, number> = {}
const bump = (k: string) => { stats[k] = (stats[k] ?? 0) + 1 }

const sleep = (ms: number) => new Promise<void>(r => setTimeout(r, ms))

interface SendBody {
  notification_id?: string
  user_id?: string
  subject?: string
  message?: string
}

const app = Fastify({ logger: { level: process.env.LOG_LEVEL ?? 'warn' } })

app.get('/health', async () => ({ ok: true, channels: CHANNELS }))

app.get('/stats', async () => ({ settings, stats }))

app.post('/stats/reset', async () => {
  for (const k of Object.keys(stats)) delete stats[k]
  return { ok: true }
})

/** Live-tunable so the dashboard can change failure injection mid-run. */
app.post<{ Body: Partial<Settings> }>('/settings', async req => {
  const b = req.body ?? {}
  if (typeof b.failureRate === 'number') settings.failureRate = Math.min(Math.max(b.failureRate, 0), 1)
  if (typeof b.permanentShare === 'number') settings.permanentShare = Math.min(Math.max(b.permanentShare, 0), 1)
  if (typeof b.latencyScale === 'number') settings.latencyScale = Math.max(b.latencyScale, 0)
  if (typeof b.enabled === 'boolean') settings.enabled = b.enabled
  return settings
})

for (const channel of CHANNELS) {
  app.post<{ Body: SendBody }>(`/${channel}/send`, async (req, reply) => {
    const profile = PROFILES[channel]
    bump(`${channel}.requests`)

    // Simulate provider latency.
    const latency = (profile.baseMs + Math.random() * profile.jitterMs) * settings.latencyScale
    await sleep(latency)

    // An explicit per-request override drives the single-message console, where
    // the operator ticks "force failure" for one channel and watches retries.
    const forced = req.headers['x-force-fail']
    const forcePermanent = req.headers['x-force-permanent'] === 'true'

    let fail: boolean
    if (forced === 'true') fail = true
    else if (forced === 'false') fail = false
    else fail = !settings.enabled || Math.random() < settings.failureRate

    if (!fail) {
      bump(`${channel}.delivered`)
      return reply.code(200).send({
        ok: true,
        channel,
        provider_message_id: `${channel.toUpperCase()}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e6).toString(36)}`,
        latency_ms: Math.round(latency),
      })
    }

    const permanent = forcePermanent || (forced !== 'true' && Math.random() < settings.permanentShare)
    if (permanent) {
      bump(`${channel}.failed_permanent`)
      return reply.code(400).send({
        ok: false, channel, error: 'invalid recipient address', permanent: true,
      })
    }
    bump(`${channel}.failed_transient`)
    return reply.code(503).send({
      ok: false, channel, error: 'upstream unavailable', permanent: false,
    })
  })
}

app.listen({ port: PORT, host: '0.0.0.0' })
  .then(addr => app.log.warn(`mock-channels listening on ${addr}`))
  .catch(err => { app.log.error(err); process.exit(1) })
