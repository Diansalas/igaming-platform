import { useQuery } from '@tanstack/react-query'
import { listAdminCasinoRounds, type AdminRound } from '../../api/casinoAdmin'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'
import { roundStatusTone } from './status'

/** Renders an optional minor-units amount, or a dash when the round never reached that stage (e.g. no win on a loss, no rollback on a settled round). */
function formatOptionalMoney(amount: number | undefined, assetCode: string, decimalExponent: number): string {
  if (amount === undefined) return '—'
  return formatMoney(amount, assetCode, decimalExponent)
}

export function CasinoRoundsPage() {
  const { limit, offset, setOffset } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['casino-rounds', { limit, offset }],
    queryFn: () => listAdminCasinoRounds({ limit, offset }),
  })

  const columns: Column<AdminRound>[] = [
    { key: 'player_account_id', header: 'Player', render: (r) => <span className="font-mono text-xs">{r.player_account_id}</span> },
    {
      key: 'game',
      header: 'Game',
      render: (r) => (
        <span className="font-mono text-xs">
          {r.provider_id}/{r.provider_game_id}
        </span>
      ),
    },
    {
      key: 'provider_round_id',
      header: 'Provider round ID',
      render: (r) => (r.provider_round_id ? <span className="font-mono text-xs">{r.provider_round_id}</span> : '—'),
    },
    { key: 'asset_code', header: 'Asset', render: (r) => r.asset_code },
    { key: 'bet_amount', header: 'Bet amount', render: (r) => formatOptionalMoney(r.bet_amount, r.asset_code, r.decimal_exponent) },
    { key: 'win_amount', header: 'Win amount', render: (r) => formatOptionalMoney(r.win_amount, r.asset_code, r.decimal_exponent) },
    {
      key: 'rollback_amount',
      header: 'Rollback amount',
      render: (r) => formatOptionalMoney(r.rollback_amount, r.asset_code, r.decimal_exponent),
    },
    { key: 'status', header: 'Status', render: (r) => <Badge tone={roundStatusTone(r.status)}>{r.status}</Badge> },
    { key: 'launched_at', header: 'Launched', render: (r) => r.launched_at },
  ]

  return (
    <div>
      <PageHeader title="Casino Rounds" description="Tenant-wide casino round visibility." />
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(r) => r.session_id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No casino rounds yet."
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
