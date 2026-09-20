import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { listWithdrawals, type Withdrawal, type WithdrawalState } from '../../api/withdrawals'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'
import { WITHDRAWAL_STATUS_OPTIONS, withdrawalStatusTone } from './status'

export function WithdrawalQueuePage() {
  const navigate = useNavigate()
  const [status, setStatus] = useState<WithdrawalState | ''>('')
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['withdrawals', { status, limit, offset }],
    queryFn: () => listWithdrawals({ status: status || undefined, limit, offset }),
  })

  const columns: Column<Withdrawal>[] = [
    { key: 'player_account_id', header: 'Player', render: (w) => <span className="font-mono text-xs">{w.player_account_id}</span> },
    { key: 'amount', header: 'Amount', render: (w) => formatMoney(w.amount, w.asset_code, w.decimal_exponent) },
    { key: 'state', header: 'State', render: (w) => <Badge tone={withdrawalStatusTone(w.state)}>{w.state}</Badge> },
    { key: 'requested_at', header: 'Requested', render: (w) => w.requested_at },
  ]

  return (
    <div>
      <PageHeader title="Withdrawals" description="Full withdrawal queue for this tenant." />
      <div className="mb-4 flex flex-wrap gap-2">
        <Select
          placeholder="All states"
          options={WITHDRAWAL_STATUS_OPTIONS}
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as WithdrawalState | '')
            resetToFirstPage()
          }}
        />
      </div>
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(w) => w.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No withdrawals match this filter."
        onRowClick={(w) => navigate(`/withdrawals/${w.id}`)}
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
