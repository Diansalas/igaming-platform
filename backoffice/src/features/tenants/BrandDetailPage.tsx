import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from 'react-router-dom'
import { getBrand } from '../../api/tenants'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'

export function BrandDetailPage() {
  const { tenantId = '', brandId = '' } = useParams()

  const { data: brand, isLoading, error, refetch } = useQuery({
    queryKey: ['brand', tenantId, brandId],
    queryFn: () => getBrand(tenantId, brandId),
    enabled: !!tenantId && !!brandId,
  })

  if (isLoading) return <LoadingSpinner label="Loading brand..." />
  if (error) return <ErrorState error={error} onRetry={() => void refetch()} />
  if (!brand) return null

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={brand.name} description={`Slug: ${brand.slug}`} />
      <Card title="Brand details">
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Status</dt>
            <dd className="mt-1">
              <Badge tone={brand.status === 'active' ? 'success' : 'warning'}>{brand.status}</Badge>
            </dd>
          </div>
          <div>
            <dt className="text-slate-500">Brand ID</dt>
            <dd className="mt-1 break-all font-mono text-xs text-slate-700">{brand.id}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Tenant ID</dt>
            <dd className="mt-1 break-all font-mono text-xs text-slate-700">{brand.tenant_id}</dd>
          </div>
        </dl>
      </Card>
      <Link to={`/tenants/${tenantId}`} className="text-sm text-brand-700 hover:underline">
        &larr; Back to tenant
      </Link>
    </div>
  )
}
