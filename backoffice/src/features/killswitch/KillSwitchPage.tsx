import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { listKillSwitches, type KillSwitch } from '../../api/killSwitch'
import { listTenants } from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { EngageDialog } from './EngageDialog'
import { ActorText, KillSwitchErrorAlert, MockBanner, OperationText, ProviderText, ScopeText } from './KillSwitchBits'
import { ReleasePanel } from './ReleasePanel'

/**
 * Payment kill switch console (ADR 0114 S6). Route-gated by `killSwitch` as a convenience only; the server is
 * authoritative and every refusal is rendered through the closed code map. The API is per tenant: there is no
 * platform-wide or provider-wide route, no history route and no release-request list, so "history" below is the
 * list of released rows and the release request is reached through the switch.
 */
export function KillSwitchPage() {
  const { claims } = useAuth()
  const qc = useQueryClient()
  const platform = isPlatformAdmin(claims)
  const [pickedTenant, setPickedTenant] = useState('')
  const [search, setSearch] = useState('')
  const debounced = useDebouncedValue(search, 250)
  const tenantId = platform ? pickedTenant : (claims?.tenant_id ?? '')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [engaging, setEngaging] = useState(false)
  const [notice, setNotice] = useState('')

  const tenants = useQuery({
    queryKey: ['killswitch', 'tenants', debounced],
    queryFn: () => listTenants({ q: debounced || undefined, limit: 50 }),
    enabled: platform,
    retry: false,
  })
  const list = useQuery({
    queryKey: ['killswitch', 'list', tenantId],
    queryFn: () => listKillSwitches(tenantId),
    enabled: tenantId !== '',
    retry: false,
  })
  const rows = list.data?.kill_switches ?? []
  const engaged = rows.filter((r) => r.engaged)
  const released = rows.filter((r) => !r.engaged)
  const selected = rows.find((r) => r.id === selectedId) ?? null

  function refresh() {
    void qc.invalidateQueries({ queryKey: ['killswitch'] })
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Payment kill switch" description="Stop outbound dispatch to a payment provider for one tenant. Engage is immediate; release needs two people." />
      <MockBanner />
      {platform ? (
        <div className="flex flex-wrap items-end gap-2">
          <TextInput label="Find tenant" value={search} onChange={(e) => setSearch(e.target.value)} autoComplete="off" />
          <Select
            aria-label="Tenant"
            placeholder="Choose a tenant"
            options={(tenants.data?.items ?? []).map((t) => ({ value: t.id, label: t.name }))}
            value={pickedTenant}
            onChange={(e) => { setPickedTenant(e.target.value); setSelectedId(null); setNotice('') }}
          />
          {tenants.error && <p className="text-sm text-red-700">The tenant list could not be loaded.</p>}
        </div>
      ) : (
        <p className="text-sm text-slate-600">Showing the switches for your own tenant.</p>
      )}
      {tenantId === '' && <p className="text-sm text-slate-500">Choose a tenant to see its kill switches.</p>}
      {tenantId !== '' && (
        <div className="flex gap-2">
          <Button variant="danger" onClick={() => { setNotice(''); setEngaging(true) }}>Engage kill switch</Button>
          <Button variant="secondary" onClick={() => { setNotice(''); refresh() }} isLoading={list.isFetching}>Refresh</Button>
        </div>
      )}
      {notice && (
        <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">{notice}</p>
      )}
      {list.isLoading && <LoadingSpinner label="Loading kill switches..." />}
      {list.error && <KillSwitchErrorAlert error={list.error} op="list" />}
      {list.data && (
        <>
          <SwitchTable title="Engaged now" rows={engaged} selectedId={selectedId} onSelect={setSelectedId} empty="Nothing is engaged for this tenant." />
          <SwitchTable title="Released (history)" rows={released} selectedId={selectedId} onSelect={setSelectedId} empty="No released switches." />
          <p className="text-xs text-slate-500">
            The API returns one current row per provider and operation, including released ones. It has no per-change history; the audit log is the full record.
          </p>
        </>
      )}
      {selected && (
        <section aria-label="Switch detail" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
          <div className="flex items-start justify-between gap-2">
            <h2 className="text-sm font-semibold text-slate-900">Switch detail</h2>
            <button type="button" className="text-xs text-slate-500 hover:underline" onClick={() => setSelectedId(null)}>Close detail</button>
          </div>
          <dl className="mt-3 grid grid-cols-[11rem_1fr] gap-y-1 text-sm">
            <dt className="text-slate-500">Provider</dt>
            <dd><ProviderText value={selected.provider_scope} /></dd>
            <dt className="text-slate-500">Operation</dt>
            <dd><OperationText value={selected.operation_scope} /></dd>
            <dt className="text-slate-500">State</dt>
            <dd>{selected.engaged ? 'Engaged' : 'Released'}</dd>
            <dt className="text-slate-500">Engaged by</dt>
            <dd>{selected.engaged_by_scope ? <ScopeText value={selected.engaged_by_scope} /> : 'Not applicable'}</dd>
            <dt className="text-slate-500">Reason code</dt>
            <dd className="font-mono text-xs">{selected.reason_code}</dd>
            <dt className="text-slate-500">Last changed by</dt>
            <dd><ActorText id={selected.changed_by} scope={selected.changed_by_scope} /></dd>
            <dt className="text-slate-500">Last changed</dt>
            <dd>{new Date(selected.changed_at).toLocaleString()}</dd>
            <dt className="text-slate-500">Version</dt>
            <dd>{selected.version}</dd>
            <dt className="text-slate-500">Switch ID</dt>
            <dd className="font-mono text-xs">{selected.id}</dd>
          </dl>
          <div className="mt-4">
            <ReleasePanel
              tenantId={tenantId}
              ks={selected}
              viewerIsPlatform={platform}
              viewerId={claims?.sub}
              onChanged={refresh}
              onRefused={refresh}
              onNotice={setNotice}
            />
          </div>
        </section>
      )}
      {engaging && (
        <EngageDialog
          tenantId={tenantId}
          onClose={() => setEngaging(false)}
          onRefused={refresh}
          onDone={(ks) => {
            setEngaging(false)
            setSelectedId(ks.id)
            setNotice('Kill switch engaged. Outbound dispatch is stopped for that scope. No funds were released and no withdrawal was cancelled.')
            refresh()
          }}
        />
      )}
    </div>
  )
}

function SwitchTable({ title, rows, selectedId, onSelect, empty }: { title: string; rows: KillSwitch[]; selectedId: string | null; onSelect: (id: string) => void; empty: string }) {
  return (
    <section aria-label={title}>
      <h2 className="mb-1 text-sm font-semibold text-slate-900">{title}</h2>
      {rows.length === 0 ? (
        <p className="text-sm text-slate-500">{empty}</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">{title}</caption>
            <thead className="bg-surface-alt text-xs text-slate-500">
              <tr>
                <th className="px-3 py-2 font-medium">Provider</th>
                <th className="px-3 py-2 font-medium">Operation</th>
                <th className="px-3 py-2 font-medium">State</th>
                <th className="px-3 py-2 font-medium">Engaged by</th>
                <th className="px-3 py-2 font-medium">Changed by</th>
                <th className="px-3 py-2 font-medium">Changed</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id} className={`border-t border-border hover:bg-surface-alt ${r.id === selectedId ? 'bg-surface-alt' : ''}`}>
                  <td className="px-3 py-2">
                    <button type="button" className="text-left text-brand-700 hover:underline" onClick={() => onSelect(r.id)}>
                      <ProviderText value={r.provider_scope} />
                    </button>
                  </td>
                  <td className="px-3 py-2"><OperationText value={r.operation_scope} /></td>
                  <td className="px-3 py-2">
                    <Badge tone={r.engaged ? 'danger' : 'success'}>{r.engaged ? 'Engaged' : 'Released'}</Badge>
                    {r.engaged && r.release_request_id ? <span className="ml-2 text-xs text-slate-500">release requested</span> : null}
                  </td>
                  <td className="px-3 py-2">{r.engaged_by_scope ? <ScopeText value={r.engaged_by_scope} /> : '-'}</td>
                  <td className="px-3 py-2"><ActorText id={r.changed_by} scope={r.changed_by_scope} /></td>
                  <td className="px-3 py-2">{new Date(r.changed_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
