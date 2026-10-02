'use client'
import Link from 'next/link'
import { usePathname } from 'next/navigation'
import { useQuery } from '@tanstack/react-query'
import * as api from '@/lib/api'
import clsx from 'clsx'

const LINKS = [
  { href: '/notifications', label: 'Dashboard' },
  { href: '/load', label: 'Bulk Load' },
  { href: '/send', label: 'Send' },
  { href: '/config', label: 'Configuration' },
]

/**
 * Tenant selector, populated from the `tenants` collection.
 *
 * The pipeline binds to one tenant at startup (CB config `TENANT_ID`) and every
 * query is scoped to it server-side, so a tenant other than the active one
 * cannot be served without threading tenancy through the whole API. Rather than
 * offer a control that silently does nothing, non-active tenants are listed but
 * disabled with the reason. With the demo's single tenant this is invisible.
 */
function TenantPicker () {
  const q = useQuery({ queryKey: ['tenants'], queryFn: api.getTenants, staleTime: 60_000 })
  const active = q.data?.active
  const tenants = q.data?.tenants ?? []

  if (!tenants.length) {
    return <span className="text-xs text-muted font-mono">{active ? `tenant ${active}` : ''}</span>
  }
  return (
    <label className="flex items-center gap-2">
      <span className="text-xs text-muted">tenant</span>
      <select
        value={active ?? ''}
        onChange={() => { /* single-tenant: no other option is selectable */ }}
        className="bg-bg border border-border rounded px-2 py-1 text-xs font-mono
                   text-text focus:outline-none focus:border-accent cursor-pointer"
        title={tenants.length === 1
          ? tenants[0].name
          : 'The pipeline is bound to one tenant at startup'}>
        {tenants.map(t => (
          <option key={t.tenant_id} value={t.tenant_id} disabled={t.tenant_id !== active}>
            {t.tenant_id}{t.name ? ` — ${t.name}` : ''}
          </option>
        ))}
      </select>
    </label>
  )
}

export function Nav () {
  const path = usePathname()
  return (
    <header className="border-b border-border bg-panel/60 backdrop-blur sticky top-0 z-20">
      <div className="mx-auto max-w-[1600px] px-6 h-14 flex items-center gap-8">
        <div className="font-semibold tracking-tight">
          Couchbase <span className="text-muted font-normal">Notification Platform</span>
        </div>
        <nav className="flex gap-1">
          {LINKS.map(l => {
            // No entry points at '/' any more - it redirects to /notifications -
            // so the exact-match special case that '/' needed is gone.
            const active = path.startsWith(l.href)
            return (
              <Link key={l.href} href={l.href}
                className={clsx('px-3 py-1.5 rounded text-sm transition-colors',
                  active ? 'bg-accent/15 text-accent' : 'text-muted hover:text-text')}>
                {l.label}
              </Link>
            )
          })}
        </nav>
        <div className="ml-auto">
          <TenantPicker />
        </div>
      </div>
    </header>
  )
}
