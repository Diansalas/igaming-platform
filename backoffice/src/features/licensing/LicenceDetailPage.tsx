import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  getJurisdiction,
  getLicence,
  listLicenceCountryCeilings,
  listLicenceVersions,
  type LicenceCountryCeiling,
  type LicenceVersion,
} from '../../api/licensing'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Checkbox } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { FILING_KIND_LABEL, FileChangeDialog, type FilingKind } from './FileChangeDialog'
import { CeilingDialog } from './MarketDialogs'
import { EvidenceNote, LicenceStatusBadge, LicensingErrorAlert, short } from './LicensingBits'
import { dateText, licenseeLabel, safeToken, when } from './labels'

const LIFECYCLE: FilingKind[] = ['status', 'validity', 'review_due', 'evidence']

export function LicenceDetailPage() {
  const { licenceId = '' } = useParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const [dialog, setDialog] = useState<FilingKind | null>(null)
  const [ceilingDialog, setCeilingDialog] = useState<{ country: string } | null>(null)
  const [currentOnly, setCurrentOnly] = useState(true)
  const { limit, offset, setOffset, resetToFirstPage } = usePagination(20)
  const lic = useQuery({ queryKey: ['licensing', 'licence', licenceId], queryFn: () => getLicence(licenceId), enabled: !!licenceId, retry: false })
  const jur = useQuery({ queryKey: ['licensing', 'jurisdiction', lic.data?.jurisdiction_id], queryFn: () => getJurisdiction(lic.data!.jurisdiction_id), enabled: !!lic.data, retry: false })
  const versions = useQuery({ queryKey: ['licensing', 'licence-versions', licenceId], queryFn: () => listLicenceVersions(licenceId), enabled: !!licenceId, retry: false })
  const ceilings = useQuery({
    queryKey: ['licensing', 'ceilings', licenceId, currentOnly, offset],
    queryFn: () => listLicenceCountryCeilings({ licence_id: licenceId, current: currentOnly || undefined, limit, offset }),
    enabled: !!licenceId,
    retry: false,
  })

  if (lic.isLoading) return <LoadingSpinner label="Loading licence..." />
  if (lic.error) return <LicensingErrorAlert error={lic.error} />
  const l = lic.data
  if (!l) return null

  const versionCols: Column<LicenceVersion>[] = [
    { key: 'no', header: 'Version', render: (v) => v.version_no },
    { key: 'op', header: 'Operation', render: (v) => safeToken(v.operation) },
    { key: 'at', header: 'Changed', render: (v) => when(v.changed_at) },
    { key: 'by', header: 'Changed by', render: (v) => (v.changed_by ? `Platform staff (${short(v.changed_by)})` : 'Baseline') },
    { key: 'status', header: 'Status', render: (v) => safeToken(v.status) },
    { key: 'issued', header: 'Issued', render: (v) => dateText(v.issued_at) },
    { key: 'expires', header: 'Expires', render: (v) => dateText(v.expires_at) },
    { key: 'review', header: 'Review due', render: (v) => dateText(v.review_due_at) },
    { key: 'ev', header: 'Evidence reference (operator-supplied)', render: (v) => v.licence_evidence_reference ?? 'Not recorded' },
  ]
  const ceilingCols: Column<LicenceCountryCeiling>[] = [
    { key: 'cc', header: 'Country', render: (c) => safeToken(c.country_code) },
    { key: 'state', header: 'State', render: (c) => <Badge tone={c.state === 'enabled' ? 'info' : 'neutral'}>{c.state}</Badge> },
    { key: 'status', header: 'Version status', render: (c) => safeToken(c.status) },
    { key: 'ref', header: 'Authorisation reference (as entered)', render: (c) => c.authorization_reference ?? 'None' },
    { key: 'reason', header: 'Reason code', render: (c) => safeToken(c.reason_code) },
    { key: 'ver', header: 'Policy version', render: (c) => safeToken(c.policy_version) },
    { key: 'from', header: 'Effective from', render: (c) => when(c.effective_from) },
    { key: 'to', header: 'Effective to', render: (c) => (c.effective_to ? when(c.effective_to) : 'Open') },
    ...(perms.licensingRequest
      ? [{ key: 'act', header: '', render: (c: LicenceCountryCeiling) => (c.effective_to === null ? <Button variant="secondary" onClick={() => setCeilingDialog({ country: c.country_code })}>{`File a change for ${safeToken(c.country_code)}`}</Button> : null) }]
      : []),
  ]

  return (
    <div className="flex flex-col gap-4">
      <Card title={`Licence ${l.licence_number}`}>
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Status (recorded)</dt>
            <dd className="mt-1"><LicenceStatusBadge status={l.status} /></dd>
          </div>
          <div>
            <dt className="text-slate-500">Jurisdiction</dt>
            <dd className="mt-1">
              <Link className="text-brand-700 hover:underline" to={`/licensing/jurisdictions/${encodeURIComponent(l.jurisdiction_id)}`}>{jur.data ? `${jur.data.code} - ${jur.data.name}` : short(l.jurisdiction_id)}</Link>
            </dd>
          </div>
          <div>
            <dt className="text-slate-500">Licensee</dt>
            <dd className="mt-1">{licenseeLabel(l.licensee)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Issued</dt>
            <dd className="mt-1">{dateText(l.issued_at)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Expires</dt>
            <dd className="mt-1">{dateText(l.expires_at)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Review due</dt>
            <dd className="mt-1">{dateText(l.review_due_at)}</dd>
          </div>
          <div className="sm:col-span-2">
            <dt className="text-slate-500">Evidence reference (supplied by the operator)</dt>
            <dd className="mt-1 break-all">{l.licence_evidence_reference ?? 'Not recorded'}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Permitted products (as supplied)</dt>
            <dd className="mt-1">{l.permitted_products.length ? l.permitted_products.join(', ') : 'None recorded'}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Permitted markets (as supplied)</dt>
            <dd className="mt-1">{l.permitted_markets.length ? l.permitted_markets.join(', ') : 'None recorded'}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Licence ID</dt>
            <dd className="mt-1 break-all font-mono text-xs">{l.id}</dd>
          </div>
        </dl>
        <div className="mt-3"><EvidenceNote /></div>
        {perms.licensingRequest && (
          <div className="mt-4 flex flex-wrap gap-2" aria-label="Lifecycle changes">
            {LIFECYCLE.map((k) => (
              <Button key={k} variant="secondary" onClick={() => setDialog(k)}>{FILING_KIND_LABEL[k]}</Button>
            ))}
          </div>
        )}
        <p className="mt-2 text-xs text-slate-500">Changes that widen are filed for approval by two different people; the server decides which kind a change is.</p>
      </Card>

      <Card title="Tenants bound to this licence">
        {l.tenant_ids.length === 0 ? (
          <p className="text-sm text-slate-600">No tenant is bound to this licence.</p>
        ) : (
          <ul className="flex flex-col gap-1 text-sm">
            {l.tenant_ids.map((t) => (
              <li key={t}>
                <Link className="font-mono text-xs text-brand-700 hover:underline" to={`/licensing/tenants/${encodeURIComponent(t)}`}>{t}</Link>
              </li>
            ))}
          </ul>
        )}
      </Card>

      <Card title="Country ceilings">
        <p className="mb-2 text-xs text-slate-600">Stored ceiling versions only. This is not a decision about where operation is permitted; that is resolved by the runtime market gate.</p>
        {perms.licensingRequest && (
          <div className="mb-2">
            <Button onClick={() => setCeilingDialog({ country: '' })}>File a country ceiling change</Button>
          </div>
        )}
        <div className="mb-2">
          <Checkbox label="Current versions only" checked={currentOnly} onChange={(e) => { setCurrentOnly(e.target.checked); resetToFirstPage() }} />
        </div>
        {ceilings.error ? <LicensingErrorAlert error={ceilings.error} /> : null}
        <Table
          columns={ceilingCols}
          rows={ceilings.data?.items ?? []}
          getRowKey={(c) => c.id}
          isLoading={ceilings.isLoading}
          emptyMessage="No country ceilings are recorded for this licence."
          pagination={ceilings.data ? { limit: ceilings.data.limit, offset: ceilings.data.offset, total: ceilings.data.total, onPageChange: setOffset } : undefined}
        />
      </Card>

      <Card title="Version history">
        {versions.error ? <LicensingErrorAlert error={versions.error} /> : null}
        <Table columns={versionCols} rows={versions.data ?? []} getRowKey={(v) => String(v.version_no)} isLoading={versions.isLoading} emptyMessage="No history rows." />
      </Card>

      <Link to="/licensing/licences" className="text-sm text-brand-700 hover:underline">&larr; Back to licences</Link>
      {dialog && <FileChangeDialog initialKind={dialog} licence={l} onClose={() => setDialog(null)} />}
      {ceilingDialog && <CeilingDialog licence={l} initialCountry={ceilingDialog.country} onClose={() => setCeilingDialog(null)} />}
    </div>
  )
}
