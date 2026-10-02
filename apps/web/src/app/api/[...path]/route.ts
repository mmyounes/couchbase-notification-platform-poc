/**
 * Runtime proxy from the browser origin to the pipeline.
 *
 * This replaces a next.config `rewrites()` entry deliberately. Rewrites are
 * evaluated at BUILD time for `output: 'standalone'`, so the upstream host is
 * baked into the image: changing INTERNAL_API_URL afterwards has no effect, and
 * an image built against an older topology keeps dialling a host that no longer
 * exists (observed as ENOTFOUND 'nginx' after the pipeline was consolidated).
 *
 * A route handler resolves the target per request, so the same image works
 * wherever it is deployed.
 *
 * Proxying through this origin also means the browser only needs port 3000
 * open, and every call is same-origin so CORS never applies.
 */

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

const upstream = () => process.env.INTERNAL_API_URL ?? 'http://pipeline:8080'

async function proxy (req: Request, path: string[]): Promise<Response> {
  const url = new URL(req.url)
  const target = `${upstream()}/${path.join('/')}${url.search}`

  const init: RequestInit = {
    method: req.method,
    headers: { 'Content-Type': req.headers.get('content-type') ?? 'application/json' },
    // A broad search over 500M can legitimately take tens of seconds.
    signal: AbortSignal.timeout(120_000),
  }
  if (req.method !== 'GET' && req.method !== 'HEAD') {
    init.body = await req.text()
  }

  try {
    const res = await fetch(target, init)
    const body = await res.text()
    return new Response(body, {
      status: res.status,
      headers: { 'Content-Type': res.headers.get('content-type') ?? 'application/json' },
    })
  } catch (err) {
    // Surface the upstream failure as JSON so the UI's ErrorNote can show it,
    // rather than a bare 500 HTML page that says nothing.
    return Response.json(
      { error: `pipeline unreachable at ${upstream()}: ${err instanceof Error ? err.message : String(err)}` },
      { status: 502 },
    )
  }
}

type Ctx = { params: Promise<{ path: string[] }> }

export async function GET (req: Request, ctx: Ctx) {
  return proxy(req, (await ctx.params).path)
}
export async function POST (req: Request, ctx: Ctx) {
  return proxy(req, (await ctx.params).path)
}
export async function PUT (req: Request, ctx: Ctx) {
  return proxy(req, (await ctx.params).path)
}
export async function DELETE (req: Request, ctx: Ctx) {
  return proxy(req, (await ctx.params).path)
}
