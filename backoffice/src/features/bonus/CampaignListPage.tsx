import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { listCampaigns, type Campaign, type CampaignStatus } from '../../api/bonus'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { CAMPAIGN_STATUS_OPTIONS, campaignStatusTone } from './status'

export function CampaignListPage() {
  const [status, setStatus] = useState<CampaignStatus | ''>('')
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['bonus-campaigns', { status, limit, offset }],
    queryFn: () => listCampaigns({ status: status || undefined, limit, offset }),
  })

  const columns: Column<Campaign>[] = [
    { key: 'id', header: 'Campaign', render: (c) => <span className="font-mono text-xs">{c.id}</span> },
    { key: 'status', header: 'Status', render: (c) => <Badge tone={campaignStatusTone(c.status)}>{c.status}</Badge> },
    { key: 'fulfillment_owner', header: 'Fulfillment owner', render: (c) => c.fulfillment_owner },
    { key: 'created_at', header: 'Created', render: (c) => c.created_at ?? '—' },
  ]

  return (
    <div>
      <PageHeader title="Bonus campaigns" description="Read-only visibility - authoring/activation is a Promotions Manager action." />
      <div className="mb-4 flex flex-wrap gap-2">
        <Select
          placeholder="All statuses"
          options={CAMPAIGN_STATUS_OPTIONS}
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as CampaignStatus | '')
            resetToFirstPage()
          }}
        />
      </div>
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(c) => c.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No campaigns match this filter."
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
