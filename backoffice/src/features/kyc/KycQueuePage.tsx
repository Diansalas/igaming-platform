import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { listKycCases, type KycCase, type KycStatus } from '../../api/kyc'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { KycCaseDetail } from './KycCaseDetail'
import { KYC_STATUS_OPTIONS, kycStatusTone } from './status'

export function KycQueuePage() {
  const [status, setStatus] = useState<KycStatus | ''>('')
  const [selected, setSelected] = useState<KycCase | null>(null)
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['kyc-cases', { status, limit, offset }],
    queryFn: () => listKycCases({ status: status || undefined, limit, offset }),
  })

  const columns: Column<KycCase>[] = [
    { key: 'player_account_id', header: 'Player', render: (c) => <span className="font-mono text-xs">{c.player_account_id}</span> },
    { key: 'status', header: 'Status', render: (c) => <Badge tone={kycStatusTone(c.status)}>{c.status}</Badge> },
    { key: 'provider_id', header: 'Provider', render: (c) => c.provider_id },
    { key: 'submitted_at', header: 'Submitted', render: (c) => c.submitted_at ?? '—' },
    { key: 'updated_at', header: 'Updated', render: (c) => c.updated_at },
  ]

  return (
    <div>
      <PageHeader title="KYC case queue" description="Tenant-wide verification cases." />
      <div className="mb-4 flex flex-wrap gap-2">
        <Select
          placeholder="All statuses"
          options={KYC_STATUS_OPTIONS}
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as KycStatus | '')
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
        emptyMessage="No KYC cases match this filter."
        onRowClick={(c) => setSelected(c)}
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
      {selected && <KycCaseDetail kycCase={selected} onClose={() => setSelected(null)} />}
    </div>
  )
}
