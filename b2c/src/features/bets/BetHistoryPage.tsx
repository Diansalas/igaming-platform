import { useQuery } from '@tanstack/react-query'
import { listMyBets, type Bet } from '../../api/sportsbook'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Card } from '../../components/Card'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Pagination } from '../../components/Pagination'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'
import { formatDecimalOdds } from '../../lib/odds'

/**
 * `sportsbook_bets.status` values (docs/decisions/0088 §3.1) - never a
 * client-side settlement decision, purely a label for what the server
 * already decided.
 */
function statusTone(status: string): BadgeTone {
  switch (status) {
    case 'settled_won':
      return 'success'
    case 'settled_lost':
    case 'void':
      return 'danger'
    case 'open':
      return 'info'
    default:
      return 'neutral'
  }
}

function statusLabel(status: string): string {
  switch (status) {
    case 'settled_won':
      return 'Won'
    case 'settled_lost':
      return 'Lost'
    case 'void':
      return 'Void'
    case 'open':
      return 'Open'
    default:
      return status
  }
}

function formatPlacedAt(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

/** `settled_at` is null/absent for an open bet and for older cached data. */
function formatSettledAt(iso: string | null | undefined): string {
  if (!iso) return '—'
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

/**
 * Payout is only meaningful for a won bet - `payout_amount` is null for
 * open/lost/void. Rendered with the same `formatMoney` helper as every
 * other amount, never a re-derived value.
 */
function formatPayout(bet: Bet): string {
  if (bet.outcome !== 'won' || bet.payout_amount == null) return '—'
  return formatMoney(bet.payout_amount, bet.asset_code)
}

/**
 * Paginated player bet history, using the same {items,limit,offset,total}
 * envelope/pagination component as every other paginated list in this
 * codebase (see backoffice's identical convention). Renders exactly the
 * read-only fields the server returns (docs/decisions/0088 §3.4) - no
 * settlement/void/rollback controls exist here; those actions are server-
 * driven (in-house MOCK mode) and never triggered from this UI.
 */
export function BetHistoryPage() {
  const { limit, offset, setOffset } = usePagination(20)
  const query = useQuery({ queryKey: ['bets', limit, offset], queryFn: () => listMyBets({ limit, offset }) })

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Bet history" description="Your own past bets." />

      {query.isLoading && <LoadingSpinner label="Loading bets..." />}
      {query.error && <ErrorState error={query.error} onRetry={() => void query.refetch()} />}

      {query.data && query.data.items.length === 0 && <EmptyState message="You haven't placed any bets yet." />}

      {query.data && query.data.items.length > 0 && (
        <Card>
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-xs uppercase text-slate-500">
                <tr>
                  <th className="py-2 pr-4">Placed</th>
                  <th className="py-2 pr-4">Stake</th>
                  <th className="py-2 pr-4">Odds</th>
                  <th className="py-2 pr-4">Potential return</th>
                  <th className="py-2 pr-4">Status</th>
                  <th className="py-2 pr-4">Payout</th>
                  <th className="py-2 pr-4">Settled</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {query.data.items.map((bet) => (
                  <tr key={bet.id}>
                    <td className="py-2 pr-4 text-slate-600">{formatPlacedAt(bet.placed_at)}</td>
                    <td className="py-2 pr-4">{formatMoney(bet.stake_amount, bet.asset_code)}</td>
                    <td className="py-2 pr-4">{formatDecimalOdds(bet.odds_numerator, bet.odds_denominator)}</td>
                    <td className="py-2 pr-4 font-medium">{formatMoney(bet.potential_return, bet.asset_code)}</td>
                    <td className="py-2 pr-4">
                      <Badge tone={statusTone(bet.status)}>{statusLabel(bet.status)}</Badge>
                    </td>
                    <td className="py-2 pr-4 font-medium">{formatPayout(bet)}</td>
                    <td className="py-2 pr-4 text-slate-600">{formatSettledAt(bet.settled_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination limit={query.data.limit} offset={query.data.offset} total={query.data.total} onPageChange={setOffset} />
        </Card>
      )}
    </div>
  )
}
