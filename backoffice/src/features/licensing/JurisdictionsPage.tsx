import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { getJurisdiction, listJurisdictions, listLicences, type Jurisdiction } from '../../api/licensing'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { JurisdictionDialog } from './MarketDialogs'
import { Card } from '../../components/Card'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Table, type Column } from '../../components/Table'
import { TextInput } from '../../components/FormField'
import { LicenceStatusBadge, LicensingErrorAlert } from './LicensingBits'
import { when } from './labels'

export function JurisdictionsPage() {
  const [q, setQ] = useState('')
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const [creating, setCreating] = useState(false)
  const list = useQuery({ queryKey: ['licensing', 'jurisdictions'], queryFn: listJurisdictions, retry: false })
  const needle = q.trim().toLowerCase()
  const rows = (list.data ?? []).filter((j) => needle === '' || `${j.code} ${j.name} ${j.regulatory_body ?? ''}`.toLowerCase().includes(needle))
  const columns: Column<Jurisdiction>[] = [
    { key: 'code', header: 'Code', render: (j) => <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/jurisdictions/${encodeURIComponent(j.id)}`}>{j.code}</Link> },
    { key: 'name', header: 'Name', render: (j) => j.name },
    { key: 'body', header: 'Regulatory body (as recorded)', render: (j) => j.regulatory_body || 'Not recorded' },
    { key: 'created', header: 'Recorded', render: (j) => when(j.created_at) },
  ]
  return (
    <div className="flex flex-col gap-3">
      <TextInput label="Filter jurisdictions" value={q} onChange={(e) => setQ(e.target.value)} autoComplete="off" />
      {perms.licensingRequest && (
        <div>
          <Button onClick={() => setCreating(true)}>Create a jurisdiction</Button>
        </div>
      )}
      {list.error ? <LicensingErrorAlert error={list.error} /> : null}
      <Table columns={columns} rows={rows} getRowKey={(j) => j.id} isLoading={list.isLoading} emptyMessage="No jurisdictions are recorded." />
      {creating && <JurisdictionDialog onClose={() => setCreating(false)} />}
    </div>
  )
}

export function JurisdictionDetailPage() {
  const { jurisdictionId = '' } = useParams()
  const j = useQuery({ queryKey: ['licensing', 'jurisdiction', jurisdictionId], queryFn: () => getJurisdiction(jurisdictionId), enabled: !!jurisdictionId, retry: false })
  const licences = useQuery({ queryKey: ['licensing', 'licences'], queryFn: listLicences, retry: false })
  if (j.isLoading) return <LoadingSpinner label="Loading jurisdiction..." />
  if (j.error) return <LicensingErrorAlert error={j.error} />
  if (!j.data) return null
  const mine = (licences.data ?? []).filter((l) => l.jurisdiction_id === j.data.id)
  return (
    <div className="flex flex-col gap-4">
      <Card title={`${j.data.code} - ${j.data.name}`}>
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <div>
            <dt className="text-slate-500">Regulatory body (as recorded)</dt>
            <dd className="mt-1">{j.data.regulatory_body || 'Not recorded'}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Recorded</dt>
            <dd className="mt-1">{when(j.data.created_at)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Jurisdiction ID</dt>
            <dd className="mt-1 break-all font-mono text-xs">{j.data.id}</dd>
          </div>
        </dl>
        {j.data.notes ? <p className="mt-3 whitespace-pre-wrap text-sm text-slate-700">{j.data.notes}</p> : null}
        <p className="mt-3 text-xs text-slate-500">Read-only reference data. A jurisdiction row is a record; it is not an approval of the jurisdiction.</p>
      </Card>
      <Card title="Licences recorded under this jurisdiction">
        {licences.error ? <LicensingErrorAlert error={licences.error} /> : null}
        {mine.length === 0 ? (
          <p className="text-sm text-slate-600">No licences are recorded under this jurisdiction.</p>
        ) : (
          <ul className="flex flex-col gap-1 text-sm">
            {mine.map((l) => (
              <li key={l.id} className="flex items-center gap-2">
                <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/licences/${encodeURIComponent(l.id)}`}>{l.licence_number}</Link>
                <LicenceStatusBadge status={l.status} />
              </li>
            ))}
          </ul>
        )}
      </Card>
      <Link to="/licensing/jurisdictions" className="text-sm text-brand-700 hover:underline">&larr; Back to jurisdictions</Link>
    </div>
  )
}
