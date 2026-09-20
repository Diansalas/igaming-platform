import { useQuery } from '@tanstack/react-query'
import { listMyBets } from '../../api/sportsbook'
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

function statusTone(status: string): BadgeTone {
  switch (status) {
    case 'won':
      return 'success'
    case 'lost':
    case 'void':
      return 'danger'
    case 'open':
      return 'info'
    default:
      return 'neutral'
  }
}

function formatPlacedAt(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

/**
 * Paginated player bet history, using the same {items,limit,offset,total}
 * envelope/pagination component as every other paginated list in this
 * codebase (see backoffice's identical convention). Renders exactly the
 * bet fields the server returns - no settlement/cashout UI, matching this
 * stage's scope (bets stay "open" server-side).
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
                      <Badge tone={statusTone(bet.status)}>{bet.status}</Badge>
                    </td>
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
