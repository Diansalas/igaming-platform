import { useQuery } from '@tanstack/react-query'
import { listAdminSportsbookBets, type AdminBet, type SettlementLifecycleEvent } from '../../api/sportsbookAdmin'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'
import { betStatusLabel, betStatusTone, formatOdds } from './status'

function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  return iso
}

/** Payout is only meaningful for a won outcome; null otherwise. */
function formatPayout(bet: AdminBet): string {
  if (bet.outcome !== 'won' || bet.payout_amount == null) return '—'
  return formatMoney(bet.payout_amount, bet.asset_code, bet.decimal_exponent)
}

function outcomeLabel(outcome: AdminBet['outcome']): string {
  if (outcome === 'won') return 'Won'
  if (outcome === 'lost') return 'Lost'
  return '—'
}

function lifecycleEventLabel(kind: SettlementLifecycleEvent['event_kind']): string {
  switch (kind) {
    case 'settlement':
      return 'Settlement'
    case 'rollback':
      return 'Rollback'
    case 'void':
      return 'Void'
    case 'tombstone':
      return 'Tombstone'
    default:
      return kind
  }
}

/**
 * Read-only per-bet lifecycle from `sportsbook_bet_settlements`
 * (docs/decisions/0088 §3.2/§3.4). No settle/void/rollback control is
 * rendered anywhere in this component - this is history display only.
 */
function LifecycleDetails({ bet }: { bet: AdminBet }) {
  const lifecycle = bet.lifecycle ?? []
  if (lifecycle.length === 0) {
    return <span className="text-slate-400">—</span>
  }
  return (
    <details>
      <summary className="cursor-pointer text-brand-700">
        Lifecycle ({lifecycle.length})
      </summary>
      <ul className="mt-2 flex flex-col gap-2 text-xs">
        {lifecycle.map((event) => (
          <li key={event.id} className="rounded border border-border p-2">
            <div className="font-medium">
              {lifecycleEventLabel(event.event_kind)}
              {event.generation != null ? ` · gen ${event.generation}` : ''}
            </div>
            {event.event_kind === 'settlement' && (
              <div>
                Outcome: {outcomeLabel(event.outcome)}
                {event.outcome === 'won' && event.payout_amount != null
                  ? ` · Payout: ${formatMoney(event.payout_amount, bet.asset_code, bet.decimal_exponent)}`
                  : ''}
              </div>
            )}
            {event.event_kind === 'void' && event.void_reason && <div>Reason: {event.void_reason}</div>}
            <div className="font-mono text-slate-500">Ledger tx: {event.ledger_transaction_id}</div>
            <div className="text-slate-500">{formatDateTime(event.created_at)}</div>
          </li>
        ))}
      </ul>
    </details>
  )
}

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
    {
      key: 'provider_ref',
      header: 'Provider reference',
      render: (b) => (b.provider_id || b.provider_bet_reference ? <span className="font-mono text-xs">{b.provider_id}/{b.provider_bet_reference}</span> : '—'),
    },
    { key: 'status', header: 'Status', render: (b) => <Badge tone={betStatusTone(b.status)}>{betStatusLabel(b.status)}</Badge> },
    { key: 'outcome', header: 'Outcome', render: (b) => outcomeLabel(b.outcome) },
    { key: 'payout', header: 'Payout', render: (b) => formatPayout(b) },
    { key: 'settled_at', header: 'Settled', render: (b) => formatDateTime(b.settled_at) },
    { key: 'placed_at', header: 'Placed', render: (b) => b.placed_at },
    { key: 'lifecycle', header: 'Lifecycle', render: (b) => <LifecycleDetails bet={b} /> },
  ]

  return (
    <div>
      <PageHeader title="Sportsbook Bets" description="Tenant-wide sportsbook bet visibility (read-only; no settlement controls)." />
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
