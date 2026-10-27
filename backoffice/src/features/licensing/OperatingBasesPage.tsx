import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { listOperatingBases, type OperatingBasis } from '../../api/operatingBases'
import { getTenant } from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Checkbox, TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { BasisDisclaimer, BasisStatusBadge, ReviewIndicator } from './BasisBits'
import { FileBasisDialog } from './BasisDialogs'
import { TenantPicker } from './FileChangeDialog'
import { LicensingErrorAlert } from './LicensingBits'
import { basisKindLabel } from './basisLabels'
import { safeToken } from './labels'

export const basesPath = (tenantId: string) => `/licensing/operating-bases/tenants/${encodeURIComponent(tenantId)}`

/**
 * Entry: the operator picks a tenant explicitly (no default tenant). The list lives under the tenant route so a link from the
 * tenant licensing page or a change request can go straight to it.
 */
export function OperatingBasesPage() {
  const nav = useNavigate()
  const [tenant, setTenant] = useState('')
  return (
    <div className="flex flex-col gap-4">
      <BasisDisclaimer />
      <Card title="Operating bases">
        <p className="mb-3 text-sm text-slate-700">
          An operating basis records which basis a tenant or brand relies on for a country and operation: a licence held by the platform, the partner's own licence, or a documented decision without a licence. Choose a tenant to see its bases. No tenant is selected for you.
        </p>
        <TenantPicker
          value={tenant}
          onChange={(id) => {
            setTenant(id)
            if (id) nav(basesPath(id))
          }}
        />
      </Card>
    </div>
  )
}

export function TenantOperatingBasesPage() {
  const { tenantId = '' } = useParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const page = usePagination(20)
  const [country, setCountry] = useState('')
  const [currentOnly, setCurrentOnly] = useState(true)
  const [dialog, setDialog] = useState(false)
  const countryOk = country === '' || /^[A-Z]{2}$/.test(country)

  const tenant = useQuery({ queryKey: ['licensing', 'tenant', tenantId], queryFn: () => getTenant(tenantId), enabled: !!tenantId, retry: false })
  const list = useQuery({
    queryKey: ['licensing', 'operating-bases', tenantId, country, currentOnly, page.offset],
    queryFn: () => listOperatingBases({ tenant_id: tenantId, country_code: country || undefined, current: currentOnly || undefined, limit: page.limit, offset: page.offset }),
    enabled: !!tenantId && countryOk,
    retry: false,
  })

  if (tenant.isLoading) return <LoadingSpinner label="Loading tenant..." />
  if (tenant.error) return <LicensingErrorAlert error={tenant.error} />
  const t = tenant.data
  if (!t) return null

  const columns: Column<OperatingBasis>[] = [
    { key: 'cc', header: 'Country', render: (b) => <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/operating-bases/${encodeURIComponent(b.id)}`}>{safeToken(b.country_code)}</Link> },
    { key: 'scope', header: 'Scope', render: (b) => (b.brand_id ? `Brand ${safeToken(b.brand_id).slice(0, 8)}` : 'Whole tenant') },
    { key: 'op', header: 'Operation / product', render: (b) => `${safeToken(b.operation_code)} / ${b.product_code ? safeToken(b.product_code) : 'all products'}` },
    { key: 'kind', header: 'Basis', render: (b) => basisKindLabel(b.basis_kind) },
    { key: 'status', header: 'Status', render: (b) => <BasisStatusBadge status={b.status} /> },
    { key: 'review', header: 'Review due', render: (b) => <ReviewIndicator reviewDueAt={b.review_due_at} needsRereview={b.needs_rereview} /> },
    { key: 'from', header: 'Version from', render: (b) => b.effective_from.slice(0, 10) },
    { key: 'to', header: 'Version to', render: (b) => (b.effective_to ? b.effective_to.slice(0, 10) : 'Open') },
  ]

  return (
    <div className="flex flex-col gap-4">
      <BasisDisclaimer />
      <Card title={`Operating bases of tenant ${t.name}`}>
        <p className="text-xs text-slate-600">
          Tenant <span className="font-mono">{t.id}</span>. These are stored versions only; they are not a decision about where the tenant may operate. A tenant with no basis row for a scope is decided by its licence chain as before.
        </p>
        <div className="mt-3 flex flex-wrap items-end gap-3">
          <TextInput label="Country filter" value={country} onChange={(e) => { setCountry(e.target.value.toUpperCase()); page.resetToFirstPage() }} autoComplete="off" maxLength={2} help="Two letters, or blank for all." />
          <Checkbox label="Current versions only" checked={currentOnly} onChange={(e) => { setCurrentOnly(e.target.checked); page.resetToFirstPage() }} />
          {perms.licensingRequest && <Button onClick={() => setDialog(true)}>File an operating basis</Button>}
        </div>
        {!countryOk && <p role="alert" className="mt-2 text-sm text-red-700">Use two upper-case letters.</p>}
      </Card>
      {list.error ? <LicensingErrorAlert error={list.error} /> : null}
      <Table
        columns={columns}
        rows={list.data?.items ?? []}
        getRowKey={(b) => b.id}
        isLoading={list.isLoading}
        emptyMessage="No operating basis is recorded for this tenant. Nothing is assumed in its place."
        pagination={list.data ? { limit: list.data.limit, offset: list.data.offset, total: list.data.total, onPageChange: page.setOffset } : undefined}
      />
      <Link to={`/licensing/tenants/${encodeURIComponent(tenantId)}`} className="text-sm text-brand-700 hover:underline">&larr; Back to tenant licensing</Link>
      {dialog && <FileBasisDialog tenantId={tenantId} onClose={() => setDialog(false)} />}
    </div>
  )
}

