import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { listPlayers, type PlayerAccount } from '../../api/players'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { SearchInput } from '../../components/SearchInput'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { PLAYER_STATUS_OPTIONS, playerStatusTone } from './status'

export function PlayerListPage() {
  const navigate = useNavigate()
  const [q, setQ] = useState('')
  const [status, setStatus] = useState('')
  const debouncedQ = useDebouncedValue(q, 300)
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['players', { q: debouncedQ, status, limit, offset }],
    queryFn: () => listPlayers({ q: debouncedQ || undefined, status: status || undefined, limit, offset }),
  })

  const columns: Column<PlayerAccount>[] = [
    { key: 'email', header: 'Email', render: (p) => <span className="font-medium text-slate-900">{p.email}</span> },
    { key: 'status', header: 'Status', render: (p) => <Badge tone={playerStatusTone(p.status)}>{p.status}</Badge> },
    { key: 'kyc_tier', header: 'KYC tier', render: (p) => p.kyc_tier },
  ]

  return (
    <div>
      <PageHeader title="Players" description="Search and manage players in your tenant." />
      <div className="mb-4 flex flex-wrap gap-2">
        <SearchInput
          placeholder="Search by email"
          value={q}
          onChange={(e) => {
            setQ(e.target.value)
            resetToFirstPage()
          }}
        />
        <Select
          placeholder="All statuses"
          options={PLAYER_STATUS_OPTIONS}
          value={status}
          onChange={(e) => {
            setStatus(e.target.value)
            resetToFirstPage()
          }}
        />
      </div>
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(p) => p.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No players match this filter."
        onRowClick={(p) => navigate(`/players/${p.id}`)}
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
