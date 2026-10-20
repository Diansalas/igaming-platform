import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { getLicensingChange, getJurisdiction, getLicence } from '../../api/licensing'
import { getOperatingBasis, listOperatingBases, type OperatingBasis } from '../../api/operatingBases'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Table, type Column } from '../../components/Table'
import { BASIS_EVIDENCE_NOTE, REVIEW_HINT, basisKindLabel } from './basisLabels'
import { BasisDisclaimer, BasisStatusBadge, NoLicenceNotice, ReviewIndicator } from './BasisBits'
import { ReviewBasisDialog, WithdrawBasisDialog } from './BasisDialogs'
import { basesPath } from './OperatingBasesPage'
import { ChangeStatusBadge, LicensingErrorAlert, short } from './LicensingBits'
import { kindLabel, safeToken, when } from './labels'

const sameScope = (a: OperatingBasis, b: OperatingBasis) =>
  a.tenant_id === b.tenant_id && a.brand_id === b.brand_id && a.country_code === b.country_code && a.operation_code === b.operation_code && a.product_code === b.product_code
const HISTORY_LIMIT = 200

function Item({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-slate-500">{label}</dt>
      <dd className="mt-1 break-words">{children}</dd>
    </div>
  )
}

export function OperatingBasisDetailPage() {
  const { basisId = '' } = useParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const sub = claims?.sub
  const [dialog, setDialog] = useState<'withdraw' | 'review' | null>(null)

  const q = useQuery({ queryKey: ['licensing', 'operating-basis', basisId], queryFn: () => getOperatingBasis(basisId), enabled: !!basisId, retry: false })
  const b = q.data
  const history = useQuery({
    queryKey: ['licensing', 'operating-basis-history', b?.tenant_id, b?.country_code],
    queryFn: () => listOperatingBases({ tenant_id: b!.tenant_id, country_code: b!.country_code, limit: HISTORY_LIMIT }),
    enabled: !!b,
    retry: false,
  })
  const change = useQuery({
    queryKey: ['licensing', 'change', b?.change_request_id],
    queryFn: () => getLicensingChange(b!.change_request_id!),
    enabled: !!b?.change_request_id,
    retry: false,
  })
  const licence = useQuery({ queryKey: ['licensing', 'licence', b?.licence_id], queryFn: () => getLicence(b!.licence_id!), enabled: !!b?.licence_id, retry: false })
  const jurisdiction = useQuery({ queryKey: ['licensing', 'jurisdiction', b?.jurisdiction_id], queryFn: () => getJurisdiction(b!.jurisdiction_id!), enabled: !!b?.jurisdiction_id, retry: false })

  if (q.isLoading) return <LoadingSpinner label="Loading operating basis..." />
  if (q.error) return <LicensingErrorAlert error={q.error} />
  if (!b) return null

  const open = b.status === 'active' && b.effective_to === null
  const versions = (history.data?.items ?? []).filter((v) => sameScope(v, b))
  const truncated = (history.data?.total ?? 0) > HISTORY_LIMIT
  const d = b.decision
  const cr = change.data?.change_request

  const versionCols: Column<OperatingBasis>[] = [
    { key: 'from', header: 'From', render: (v) => <Link className="text-brand-700 hover:underline" to={`/licensing/operating-bases/${encodeURIComponent(v.id)}`}>{when(v.effective_from)}</Link> },
    { key: 'to', header: 'To', render: (v) => (v.effective_to ? when(v.effective_to) : 'Open') },
    { key: 'status', header: 'Status', render: (v) => <BasisStatusBadge status={v.status} /> },
    { key: 'kind', header: 'Basis', render: (v) => basisKindLabel(v.basis_kind) },
    { key: 'review', header: 'Review due', render: (v) => `${v.review_due_at ?? 'Not recorded'}${v.needs_rereview ? ' (re-review flagged)' : ''}` },
    { key: 'reason', header: 'Reason code', render: (v) => safeToken(v.reason_code) },
    { key: 'chg', header: 'Change request', render: (v) => (v.change_request_id ? <Link className="text-brand-700 hover:underline" to={`/licensing/changes/${encodeURIComponent(v.change_request_id)}`}>{short(v.change_request_id)}</Link> : 'None') },
  ]

  return (
    <div className="flex flex-col gap-4">
      <BasisDisclaimer />
      {b.basis_kind === 'no_licence_documented' && <NoLicenceNotice />}
      <Card title={`Operating basis ${safeToken(b.country_code)} / ${safeToken(b.operation_code)}`}>
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <Item label="Basis">{basisKindLabel(b.basis_kind)}</Item>
          <Item label="Version status"><BasisStatusBadge status={b.status} /></Item>
          <Item label="Review due"><ReviewIndicator reviewDueAt={b.review_due_at} needsRereview={b.needs_rereview} /></Item>
          <Item label="Tenant"><Link className="text-brand-700 hover:underline" to={basesPath(b.tenant_id)}><span className="font-mono text-xs">{b.tenant_id}</span></Link></Item>
          <Item label="Brand">{b.brand_id ? <span className="font-mono text-xs">{b.brand_id}</span> : 'Whole tenant (all brands)'}</Item>
          <Item label="Country">{safeToken(b.country_code)}</Item>
          <Item label="Operation">{safeToken(b.operation_code)}</Item>
          <Item label="Product">{b.product_code ? safeToken(b.product_code) : 'All products'}</Item>
          <Item label="Products not covered">{b.restricted_product_codes.length ? b.restricted_product_codes.map(safeToken).join(', ') : 'None'}</Item>
          <Item label="Licence">
            {b.licence_id ? (
              <Link className="text-brand-700 hover:underline" to={`/licensing/licences/${encodeURIComponent(b.licence_id)}`}>{licence.data?.licence_number ?? short(b.licence_id)}</Link>
            ) : (
              'None (a basis without a licence)'
            )}
          </Item>
          <Item label="Jurisdiction relied on">
            {b.jurisdiction_id ? (
              <Link className="text-brand-700 hover:underline" to={`/licensing/jurisdictions/${encodeURIComponent(b.jurisdiction_id)}`}>{jurisdiction.data ? `${jurisdiction.data.code} - ${jurisdiction.data.name}` : short(b.jurisdiction_id)}</Link>
            ) : (
              'None'
            )}
          </Item>
          <Item label="Determination reference (as entered)"><span className="font-mono text-xs">{b.determination_reference}</span></Item>
          <Item label="Determined by (function)">{b.determined_by_role}</Item>
          <Item label="Evidence reference (supplied by the operator, not verified)">{b.basis_evidence_reference ?? 'None'}</Item>
          <Item label="Last review reference (as entered)">{b.last_review_reference ?? 'None'}</Item>
          <Item label="Restrictions note">{b.restrictions_note ? <span className="whitespace-pre-wrap">{b.restrictions_note}</span> : 'None'}</Item>
          <Item label="Reason code"><span className="font-mono text-xs">{safeToken(b.reason_code)}</span></Item>
          <Item label="Version from">{when(b.effective_from)}</Item>
          <Item label="Version to">{b.effective_to ? when(b.effective_to) : 'Open'}</Item>
          <Item label="Policy version"><span className="font-mono text-xs">{safeToken(b.policy_version)}</span></Item>
        </dl>
        <p role="note" aria-label="Basis evidence note" className="mt-3 text-xs text-slate-600">{BASIS_EVIDENCE_NOTE}</p>
        <p className="mt-1 text-xs text-slate-500">{REVIEW_HINT}</p>
        {b.needs_rereview && <p className="mt-2 text-sm text-red-800">This basis is flagged for re-review and does not permit until a review is recorded.</p>}
        {perms.licensingRequest && open && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button variant="secondary" onClick={() => setDialog('review')}>Record a review</Button>
            <Button variant="danger" onClick={() => setDialog('withdraw')}>Withdraw this basis</Button>
          </div>
        )}
        {perms.licensingRequest && !open && <p className="mt-3 text-sm text-slate-600">This is a closed or withdrawn version. Open the current version of this scope to act on it.</p>}
      </Card>

      <Card title="Decision record (derived from the executed licensing change)">
        {d === null ? (
          <p className="text-sm text-amber-900">No decision record exists for this version: it was not created by an executed licensing change (a provisioning row). The resolver does not accept such a row as an approved basis.</p>
        ) : (
          <div className="flex flex-col gap-2 text-sm">
            <p>
              Change request <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/changes/${encodeURIComponent(d.change_request_id)}`}>{short(d.change_request_id)}</Link> - {kindLabel(d.kind)} - reason code{' '}
              <span className="font-mono text-xs">{safeToken(d.reason_code)}</span>
              {cr ? <> - <ChangeStatusBadge status={cr.status} /></> : null}
            </p>
            <p>Requested by: {d.requested_by ? `Platform staff (${short(d.requested_by)})${sub && d.requested_by === sub ? ' - you' : ''}` : 'Not shown'}</p>
            <p>Executed by: {d.executed_by ? `Platform staff (${short(d.executed_by)})` : 'Not recorded'} at {when(d.executed_at)}</p>
            <div>
              <p className="font-medium">Approvers ({d.approvals.length})</p>
              {d.approvals.length === 0 ? (
                <p className="text-slate-600">None: a narrowing change by a single administrator needs no second approval.</p>
              ) : (
                <ul aria-label="Approvers" className="list-disc pl-5">
                  {d.approvals.map((a, i) => {
                    const detail = cr?.approvals.find((x) => x.approver_id === a.approver_id)
                    return (
                      <li key={`${a.approver_id ?? 'x'}-${i}`}>
                        {a.approver_id ? `Platform staff (${short(a.approver_id)})` : 'Platform staff'}
                        {detail ? <> - {when(detail.decided_at)} - reason <span className="font-mono text-xs">{safeToken(detail.reason_code)}</span></> : null}
                      </li>
                    )
                  })}
                </ul>
              )}
            </div>
            {change.error ? <p className="text-xs text-slate-600">The approval timestamps could not be loaded; the approver list above comes from the basis record.</p> : null}
          </div>
        )}
      </Card>

      <Card title="Version history of this scope">
        {history.error ? <LicensingErrorAlert error={history.error} /> : null}
        <Table columns={versionCols} rows={versions} getRowKey={(v) => v.id} isLoading={history.isLoading} emptyMessage="No other versions." />
        {truncated && <p className="mt-2 text-xs text-slate-600">Only the newest {HISTORY_LIMIT} versions for this country are searched.</p>}
      </Card>

      <Link to={basesPath(b.tenant_id)} className="text-sm text-brand-700 hover:underline">&larr; Back to this tenant's operating bases</Link>
      {dialog === 'withdraw' && <WithdrawBasisDialog basis={b} onClose={() => setDialog(null)} />}
      {dialog === 'review' && <ReviewBasisDialog basis={b} onClose={() => setDialog(null)} />}
    </div>
  )
}
