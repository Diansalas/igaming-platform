import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import {
  listPendingWithdrawals,
  listSubmittedWithdrawals,
  listWithdrawals,
  type Withdrawal,
  type WithdrawalState,
} from '../../api/withdrawals'
import { InfoNote } from '../../components/Alert'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'
import { WITHDRAWAL_STATUS_OPTIONS, withdrawalStatusTone } from './status'

type Tab = 'pending' | 'submitted' | 'history'

const TABS: { id: Tab; label: string }[] = [
  { id: 'pending', label: 'Pending review' },
  { id: 'submitted', label: 'Submitted' },
  { id: 'history', label: 'History' },
]

function buildColumns(): Column<Withdrawal>[] {
  return [
    { key: 'id', header: 'Withdrawal', render: (w) => <span className="font-mono text-xs">{w.id}</span> },
    { key: 'player_account_id', header: 'Player', render: (w) => <span className="font-mono text-xs">{w.player_account_id}</span> },
    { key: 'amount', header: 'Amount', render: (w) => formatMoney(w.amount, w.asset_code, w.decimal_exponent) },
    { key: 'state', header: 'State', render: (w) => <Badge tone={withdrawalStatusTone(w.state)}>{w.state}</Badge> },
    { key: 'requested_at', header: 'Requested', render: (w) => w.requested_at },
  ]
}

/**
 * GET /v1/admin/withdrawals - the four-eyes review queue. Opening it is
 * the backend's designated point at which `requested` withdrawals are
 * promoted to `pending_review` (closing the player's cancellation window).
 */
function PendingQueue() {
  const navigate = useNavigate()
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['withdrawals', 'pending'],
    queryFn: listPendingWithdrawals,
  })
  return (
    <div className="flex flex-col gap-4">
      <InfoNote>
        Opening this queue moves every newly <span className="font-mono">requested</span> withdrawal into{' '}
        <span className="font-mono">pending_review</span> (the player can no longer cancel it). Each withdrawal needs approvals
        from distinct, person-linked finance staff.
      </InfoNote>
      <Table
        columns={buildColumns()}
        rows={data ?? []}
        getRowKey={(w) => w.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No withdrawals are awaiting review."
        onRowClick={(w) => navigate(`/withdrawals/${w.id}`)}
      />
    </div>
  )
}

/** GET /v1/admin/withdrawals/submitted - payouts sent to a provider and awaiting resolution. */
function SubmittedQueue() {
  const navigate = useNavigate()
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['withdrawals', 'submitted'],
    queryFn: listSubmittedWithdrawals,
  })
  return (
    <div className="flex flex-col gap-4">
      <InfoNote>Withdrawals submitted to a payment provider that have not completed or failed yet. Open one to resolve it.</InfoNote>
      <Table
        columns={buildColumns()}
        rows={data ?? []}
        getRowKey={(w) => w.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No submitted withdrawals are awaiting resolution."
        onRowClick={(w) => navigate(`/withdrawals/${w.id}`)}
      />
    </div>
  )
}

/** GET /v1/admin/withdrawals/history - read-only, paged, never promotes anything. */
function HistoryList() {
  const navigate = useNavigate()
  const [status, setStatus] = useState<WithdrawalState | ''>('')
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['withdrawals', 'history', { status, limit, offset }],
    queryFn: () => listWithdrawals({ status: status || undefined, limit, offset }),
  })
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap gap-2">
        <Select
          aria-label="Filter by state"
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
        columns={buildColumns()}
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

export function WithdrawalQueuePage() {
  const [params, setParams] = useSearchParams()
  const raw = params.get('tab')
  const tab: Tab = raw === 'submitted' || raw === 'history' ? raw : 'pending'

  return (
    <div>
      <PageHeader title="Withdrawals" description="Review, approve, submit and resolve this tenant's withdrawals." />
      <div role="tablist" aria-label="Withdrawal views" className="mb-4 flex gap-1 border-b border-border">
        {TABS.map((t) => (
          <button
            key={t.id}
            role="tab"
            type="button"
            aria-selected={tab === t.id}
            onClick={() => setParams({ tab: t.id })}
            className={`-mb-px border-b-2 px-3 py-2 text-sm font-medium ${
              tab === t.id ? 'border-brand-600 text-brand-700' : 'border-transparent text-slate-500 hover:text-slate-700'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>
      {tab === 'pending' && <PendingQueue />}
      {tab === 'submitted' && <SubmittedQueue />}
      {tab === 'history' && <HistoryList />}
    </div>
  )
}
