import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { getTenant, listBrands } from '../../api/tenants'
import type { Brand } from '../../api/tenants'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { getNavPermissions } from '../../auth/permissions'
import { CreateBrandForm } from './CreateBrandForm'
import { CreateStaffForm } from './CreateStaffForm'
import { LinkStaffPersonForm } from './LinkStaffPersonForm'

export function TenantDetailPage() {
  const { tenantId = '' } = useParams()
  const navigate = useNavigate()
  const { limit, offset, setOffset } = usePagination()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)

  const tenantQuery = useQuery({
    queryKey: ['tenant', tenantId],
    queryFn: () => getTenant(tenantId),
    enabled: !!tenantId,
  })

  const brandsQuery = useQuery({
    queryKey: ['tenant-brands', tenantId, { limit, offset }],
    queryFn: () => listBrands(tenantId, { limit, offset }),
    enabled: !!tenantId,
  })

  if (tenantQuery.isLoading) return <LoadingSpinner label="Loading tenant..." />
  if (tenantQuery.error) return <ErrorState error={tenantQuery.error} onRetry={() => void tenantQuery.refetch()} />
  const tenant = tenantQuery.data
  if (!tenant) return null

  const columns: Column<Brand>[] = [
    { key: 'name', header: 'Name', render: (b) => <span className="font-medium text-slate-900">{b.name}</span> },
    { key: 'slug', header: 'Slug', render: (b) => b.slug },
    { key: 'status', header: 'Status', render: (b) => <Badge tone={b.status === 'active' ? 'success' : 'warning'}>{b.status}</Badge> },
  ]

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={tenant.name} description={`Slug: ${tenant.slug}`} />
      <Card title="Tenant details">
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Licensing model</dt>
            <dd className="mt-1 font-medium text-slate-900">{tenant.licensing_model}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Status</dt>
            <dd className="mt-1">
              <Badge tone={tenant.status === 'active' ? 'success' : 'warning'}>{tenant.status}</Badge>
            </dd>
          </div>
          <div>
            <dt className="text-slate-500">Tenant ID</dt>
            <dd className="mt-1 break-all font-mono text-xs text-slate-700">{tenant.id}</dd>
          </div>
        </dl>
      </Card>

      <Card title="Brands">
        <Table
          columns={columns}
          rows={brandsQuery.data?.items ?? []}
          getRowKey={(b) => b.id}
          isLoading={brandsQuery.isLoading}
          error={brandsQuery.error}
          onRetry={() => void brandsQuery.refetch()}
          emptyMessage="This tenant has no brands yet."
          onRowClick={(b) => navigate(`/tenants/${tenantId}/brands/${b.id}`)}
          pagination={
            brandsQuery.data
              ? { limit: brandsQuery.data.limit, offset: brandsQuery.data.offset, total: brandsQuery.data.total, onPageChange: setOffset }
              : undefined
          }
        />
      </Card>

      {perms.brandWrite && <CreateBrandForm tenantId={tenantId} />}
      {perms.staffAdmin && <CreateStaffForm tenantId={tenantId} />}
      {perms.staffAdmin && <LinkStaffPersonForm tenantId={tenantId} />}

      <Link to={isPlatformAdmin(claims) ? '/tenants' : '/'} className="text-sm text-brand-700 hover:underline">
        &larr; {isPlatformAdmin(claims) ? 'Back to tenants' : 'Back to home'}
      </Link>
    </div>
  )
}
