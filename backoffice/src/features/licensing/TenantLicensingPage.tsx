import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  listLicences,
  listOperatingCountryPolicies,
  listTenantJurisdictionConfigs,
  type OperatingCountryPolicy,
  type TenantJurisdictionConfig,
} from '../../api/licensing'
import { getTenant } from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Checkbox } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { FileChangeDialog, type FilingKind } from './FileChangeDialog'
import { Disclaimer, LicenceStatusBadge, LicensingErrorAlert, short } from './LicensingBits'
import { LICENSING_MODEL_LABEL, licenseeLabel, safeToken, when } from './labels'

const list = (a: string[]) => (a.length ? a.join(', ') : 'None')

export function TenantLicensingPage() {
  const { tenantId = '' } = useParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const [dialog, setDialog] = useState<FilingKind | null>(null)
  const [currentOnly, setCurrentOnly] = useState(true)
  const cfgPage = usePagination(20)
  const polPage = usePagination(20)

  const tenant = useQuery({ queryKey: ['licensing', 'tenant', tenantId], queryFn: () => getTenant(tenantId), enabled: !!tenantId, retry: false })
  const licences = useQuery({ queryKey: ['licensing', 'licences'], queryFn: listLicences, retry: false })
  const configs = useQuery({
    queryKey: ['licensing', 'tenant-configs', tenantId, cfgPage.offset],
    queryFn: () => listTenantJurisdictionConfigs(tenantId, { limit: cfgPage.limit, offset: cfgPage.offset }),
    enabled: !!tenantId,
    retry: false,
  })
  const policies = useQuery({
    queryKey: ['licensing', 'tenant-policies', tenantId, currentOnly, polPage.offset],
    queryFn: () => listOperatingCountryPolicies({ tenant_id: tenantId, current: currentOnly || undefined, limit: polPage.limit, offset: polPage.offset }),
    enabled: !!tenantId,
    retry: false,
  })

  if (tenant.isLoading) return <LoadingSpinner label="Loading tenant..." />
  if (tenant.error) return <LicensingErrorAlert error={tenant.error} />
  const t = tenant.data
  if (!t) return null
  const bound = (licences.data ?? []).find((l) => l.tenant_ids.includes(tenantId))

  const cfgCols: Column<TenantJurisdictionConfig>[] = [
    { key: 'jur', header: 'Jurisdiction', render: (c) => <Link className="text-brand-700 hover:underline" to={`/licensing/jurisdictions/${encodeURIComponent(c.jurisdiction_id)}`}>{short(c.jurisdiction_id)}</Link> },
    { key: 'kyc', header: 'KYC ruleset', render: (c) => c.kyc_ruleset_id ?? 'None' },
    { key: 'aml', header: 'AML ruleset', render: (c) => c.aml_ruleset_id ?? 'None' },
    { key: 'rg', header: 'RG ruleset', render: (c) => c.rg_ruleset_id ?? 'None' },
    { key: 'rep', header: 'Reporting ruleset', render: (c) => c.reporting_ruleset_id ?? 'None' },
    { key: 'cur', header: 'Currencies', render: (c) => list(c.allowed_currencies) },
    { key: 'pm', header: 'Payment methods', render: (c) => list(c.allowed_payment_methods) },
    { key: 'geo', header: 'Geo blocks', render: (c) => list(c.geo_block_list) },
    { key: 'from', header: 'Effective from', render: (c) => when(c.effective_from) },
    { key: 'to', header: 'Effective to', render: (c) => (c.effective_to ? when(c.effective_to) : 'Open') },
  ]
  const polCols: Column<OperatingCountryPolicy>[] = [
    { key: 'cc', header: 'Country', render: (p) => safeToken(p.country_code) },
    { key: 'scope', header: 'Scope', render: (p) => safeToken(p.scope) },
    { key: 'op', header: 'Operation / product', render: (p) => [p.operation_code, p.product_code].filter(Boolean).map(safeToken).join(' / ') || 'All' },
    { key: 'state', header: 'State', render: (p) => <Badge tone={p.state === 'enabled' ? 'info' : 'neutral'}>{p.state}</Badge> },
    { key: 'status', header: 'Version status', render: (p) => safeToken(p.status) },
    { key: 'ref', header: 'Authorisation reference (as entered)', render: (p) => p.authorization_reference ?? 'None' },
    { key: 'reason', header: 'Reason code', render: (p) => safeToken(p.reason_code) },
    { key: 'from', header: 'Effective from', render: (p) => when(p.effective_from) },
    { key: 'to', header: 'Effective to', render: (p) => (p.effective_to ? when(p.effective_to) : 'Open') },
  ]

  return (
    <div className="flex flex-col gap-4">
      <Disclaimer />
      <Card title={`Tenant ${t.name}`}>
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <div>
            <dt className="text-slate-500">Licensing model (set when the tenant was created)</dt>
            <dd className="mt-1">{LICENSING_MODEL_LABEL[t.licensing_model] ?? safeToken(t.licensing_model)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Tenant ID</dt>
            <dd className="mt-1 break-all font-mono text-xs">{t.id}</dd>
          </div>
        </dl>
        <p className="mt-3 text-xs text-slate-500">The licensing model is read-only here. Nothing on this page selects a model or a licence for the tenant.</p>
      </Card>

      <Card title="Licence binding">
        {licences.error ? <LicensingErrorAlert error={licences.error} /> : null}
        {licences.isLoading ? (
          <LoadingSpinner label="Loading licences..." />
        ) : bound ? (
          <div className="flex flex-col gap-1 text-sm">
            <p>
              Bound to licence{' '}
              <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/licences/${encodeURIComponent(bound.id)}`}>{bound.licence_number}</Link>{' '}
              <LicenceStatusBadge status={bound.status} />
            </p>
            <p className="text-slate-600">{licenseeLabel(bound.licensee)}</p>
          </div>
        ) : (
          <p className="text-sm text-slate-600">No licence is bound to this tenant. A tenant is never bound by default.</p>
        )}
        {perms.licensingRequest && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button onClick={() => setDialog('bind_tenant')}>{bound ? 'File a rebind to a licence' : 'File a licence binding'}</Button>
            {bound && <Button variant="secondary" onClick={() => setDialog('unbind_tenant')}>Unbind licence</Button>}
          </div>
        )}
        <p className="mt-2 text-xs text-slate-600">A binding is filed for approval by two different people and you choose the licence explicitly. The server refuses a licence whose licensee does not match the tenant's licensing model.</p>
      </Card>

      <Card title="Operating bases">
        <p className="text-sm text-slate-700">
          The basis this tenant or one of its brands relies on for a country and operation (a licence held by the platform, the partner's own licence, or a documented decision without a licence). A basis is a recorded business decision, not legal proof.
        </p>
        <p className="mt-2 text-sm">
          <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/operating-bases/tenants/${encodeURIComponent(tenantId)}`}>Open operating bases of this tenant</Link>
        </p>
      </Card>

      <Card title="Tenant jurisdiction configurations (read-only)">
        {configs.error ? <LicensingErrorAlert error={configs.error} /> : null}
        <Table
          columns={cfgCols}
          rows={configs.data?.items ?? []}
          getRowKey={(c) => c.id}
          isLoading={configs.isLoading}
          emptyMessage="No jurisdiction configuration is recorded for this tenant."
          pagination={configs.data ? { limit: configs.data.limit, offset: configs.data.offset, total: configs.data.total, onPageChange: cfgPage.setOffset } : undefined}
        />
      </Card>

      <Card title="Operating-country policies (read-only)">
        <p className="mb-2 text-xs text-slate-600">Stored policy versions only. This is not a decision about where the tenant may operate.</p>
        <div className="mb-2">
          <Checkbox label="Current versions only" checked={currentOnly} onChange={(e) => { setCurrentOnly(e.target.checked); polPage.resetToFirstPage() }} />
        </div>
        {policies.error ? <LicensingErrorAlert error={policies.error} /> : null}
        <Table
          columns={polCols}
          rows={policies.data?.items ?? []}
          getRowKey={(p) => p.id}
          isLoading={policies.isLoading}
          emptyMessage="No operating-country policies are recorded for this tenant."
          pagination={policies.data ? { limit: policies.data.limit, offset: policies.data.offset, total: policies.data.total, onPageChange: polPage.setOffset } : undefined}
        />
      </Card>

      <Link to={`/tenants/${tenantId}`} className="text-sm text-brand-700 hover:underline">&larr; Back to tenant</Link>
      {dialog && <FileChangeDialog initialKind={dialog} tenantId={tenantId} onClose={() => setDialog(null)} />}
    </div>
  )
}
