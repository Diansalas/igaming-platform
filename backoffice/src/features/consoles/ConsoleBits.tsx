import { useQuery } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import { listTenants } from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { TextInput } from '../../components/FormField'
import { Select } from '../../components/Select'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { safeCode } from '../killswitch/labels'
import { describeConsoleError, type ConsoleFamily, type ConsoleOp } from './errors'

/**
 * Fixed banner (ADR 0114 principle 6). HARDCODED on purpose: no API field says whether a payment provider is a
 * mock; every adapter in this repository is a MOCK today. When a non-mock provider exists this MUST be derived
 * from a server field (not authorised yet).
 */
export const MOCK_PAYMENTS_BANNER = 'Sandbox/MOCK provider - software capability only, not a regulatory approval.'

export function MockPaymentsBanner() {
  return (
    <div role="note" aria-label="Stack status" className="rounded-md border border-red-300 bg-red-50 px-4 py-3 text-sm text-red-900">
      <p className="font-bold">{MOCK_PAYMENTS_BANNER}</p>
      <p className="mt-1">No real payment provider is connected. What this screen shows and does is rehearsal against mock adapters.</p>
    </div>
  )
}

export function ConsoleErrorAlert({ error, family, op }: { error: unknown; family: ConsoleFamily; op: ConsoleOp }) {
  if (!error) return null
  const v = describeConsoleError(error, family, op)
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{v.message}</p>
      {(v.status || v.code) && (
        <p className="mt-1 text-xs text-red-700">
          {v.status ? `HTTP ${v.status}` : ''}
          {v.status && v.code ? ' - ' : ''}
          {v.code ? <span className="font-mono">{v.code}</span> : null}
        </p>
      )}
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {safeCode(v.requestId) === 'unprintable' ? 'unprintable' : v.requestId}</p>}
    </div>
  )
}

export function Notice({ children }: { children: ReactNode }) {
  return (
    <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">
      {children}
    </p>
  )
}

export function Hint({ children }: { children: ReactNode }) {
  return <p className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">{children}</p>
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
export const isUuid = (s: string) => UUID.test(s)

export const PII_WARNING = 'Visible to tenant staff and platform staff. Do not enter names, contact details or other personal data.'

/**
 * Tenant context for a console. Tenant-scoped staff NEVER choose a tenant: the path tenant comes from the token.
 * Only a platform principal gets a picker; the choice lives in the `tenant` search parameter (a tenant id is not a
 * secret) and must be a UUID before it is ever used in a path.
 */
export function useConsoleTenant() {
  const { claims } = useAuth()
  const platform = isPlatformAdmin(claims)
  const [sp, setSp] = useSearchParams()
  const fromUrl = sp.get('tenant') ?? ''
  const picked = isUuid(fromUrl) ? fromUrl : ''
  const tenantId = platform ? picked : (claims?.tenant_id ?? '')
  const [search, setSearch] = useState('')
  const debounced = useDebouncedValue(search, 250)
  const tenants = useQuery({
    queryKey: ['consoles', 'tenants', debounced],
    queryFn: () => listTenants({ q: debounced || undefined, limit: 50 }),
    enabled: platform,
    retry: false,
  })
  function setTenant(id: string) {
    const next = new URLSearchParams(sp)
    if (id) next.set('tenant', id)
    else next.delete('tenant')
    next.delete('attempt')
    setSp(next, { replace: true })
  }
  const options = (tenants.data?.items ?? []).map((t) => ({ value: t.id, label: t.name }))
  // Keep the picked tenant selectable even when the current search no longer lists it.
  if (picked && !options.some((o) => o.value === picked)) options.unshift({ value: picked, label: `Tenant ${picked.slice(0, 8)}` })
  const picker = platform ? (
    <div className="flex flex-wrap items-end gap-2">
      <TextInput label="Find tenant" value={search} onChange={(e) => setSearch(e.target.value)} autoComplete="off" />
      <Select aria-label="Tenant" placeholder="Choose a tenant" options={options} value={picked} onChange={(e) => setTenant(e.target.value)} />
      {tenants.error ? <p className="text-sm text-red-700">The tenant list could not be loaded.</p> : null}
    </div>
  ) : (
    <p className="text-sm text-slate-600">Showing data for your own tenant.</p>
  )
  return { claims, platform, tenantId, picker, setTenant, searchParams: sp, setSearchParams: setSp }
}

/** A short, printable form of an id (never the server's text). */
export function shortId(id: string | null | undefined): string {
  return typeof id === 'string' && /^[0-9a-fA-F-]{8,40}$/.test(id) ? id.slice(0, 8) : 'unprintable'
}

/**
 * Staff actor display (ADR 0114 principle 7 / H1). A null id with a non-tenant scope is shown as scope only; an
 * id is shown as an 8-character prefix. Never a name or email.
 */
export function actorText(id: string | null | undefined, scope: string | null | undefined): string {
  const label = scope === 'tenant' ? 'Tenant staff' : scope === 'platform' || scope === 'platform_acting' ? 'Platform staff' : 'Unrecognised scope'
  return id ? `${label} (${shortId(id)})` : label
}
