import { useQuery } from '@tanstack/react-query'
import { listRgRestrictions, type RgRestriction } from '../../api/rg'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'

export function RgQueuePage() {
  const { limit, offset, setOffset } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['rg-restrictions', { limit, offset }],
    queryFn: () => listRgRestrictions({ limit, offset }),
  })

  const columns: Column<RgRestriction>[] = [
    { key: 'restriction_type', header: 'Type', render: (r) => r.restriction_type },
    { key: 'scope', header: 'Scope', render: (r) => r.scope },
    { key: 'active', header: 'Active', render: (r) => <Badge tone={r.active ? 'warning' : 'neutral'}>{r.active ? 'Active' : 'Ended'}</Badge> },
    { key: 'indefinite', header: 'Indefinite', render: (r) => (r.indefinite ? 'Yes' : 'No') },
    { key: 'starts_at', header: 'Starts', render: (r) => r.starts_at },
    { key: 'ends_at', header: 'Ends', render: (r) => r.ends_at ?? '—' },
    { key: 'source', header: 'Source', render: (r) => r.source },
  ]

  return (
    <div>
      <PageHeader
        title="Responsible Gaming restrictions"
        description="Tenant-wide self-exclusion/restriction visibility. Read-only in this release - restriction policy is owned by internal/rg, not this UI."
      />
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(r) => r.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No restrictions recorded for this tenant."
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
