import { useQuery } from '@tanstack/react-query'
import { listAdminSportsbookBets, type AdminBet } from '../../api/sportsbookAdmin'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'
import { betStatusTone, formatOdds } from './status'

export function SportsbookBetsPage() {
  const { limit, offset, setOffset } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['sportsbook-bets', { limit, offset }],
    queryFn: () => listAdminSportsbookBets({ limit, offset }),
  })

  const columns: Column<AdminBet>[] = [
    { key: 'player_account_id', header: 'Player', render: (b) => <span className="font-mono text-xs">{b.player_account_id}</span> },
    { key: 'selection_id', header: 'Selection', render: (b) => <span className="font-mono text-xs">{b.selection_id}</span> },
    { key: 'stake', header: 'Stake', render: (b) => formatMoney(b.stake_amount, b.asset_code, b.decimal_exponent) },
    { key: 'odds', header: 'Odds', render: (b) => formatOdds(b.odds_numerator, b.odds_denominator) },
    { key: 'potential_return', header: 'Potential return', render: (b) => formatMoney(b.potential_return, b.asset_code, b.decimal_exponent) },
    { key: 'status', header: 'Status', render: (b) => <Badge tone={betStatusTone(b.status)}>{b.status}</Badge> },
    { key: 'placed_at', header: 'Placed', render: (b) => b.placed_at },
  ]

  return (
    <div>
      <PageHeader title="Sportsbook Bets" description="Tenant-wide sportsbook bet visibility." />
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(b) => b.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No sportsbook bets placed yet."
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
