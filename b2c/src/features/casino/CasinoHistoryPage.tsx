import { useQuery } from '@tanstack/react-query'
import { listCasinoRounds, type CasinoRoundStatus } from '../../api/casino'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Card } from '../../components/Card'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Pagination } from '../../components/Pagination'
import { usePagination } from '../../components/usePagination'
import { formatMoney } from '../../lib/money'

function statusTone(status: CasinoRoundStatus): BadgeTone {
  switch (status) {
    case 'won':
      return 'success'
    case 'rolled_back':
      return 'danger'
    case 'wagered':
      return 'info'
    case 'launched':
      return 'neutral'
    default:
      return 'neutral'
  }
}

function formatLaunchedAt(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

/**
 * Paginated player casino round history, same {items,limit,offset,total}
 * envelope/pagination component as bet history. Renders exactly the round
 * fields the server returns.
 */
export function CasinoHistoryPage() {
  const { limit, offset, setOffset } = usePagination(20)
  const query = useQuery({ queryKey: ['casino', 'rounds', limit, offset], queryFn: () => listCasinoRounds({ limit, offset }) })

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Casino history" description="Your own past casino rounds." />

      {query.isLoading && <LoadingSpinner label="Loading rounds..." />}
      {query.error && <ErrorState error={query.error} onRetry={() => void query.refetch()} />}

      {query.data && query.data.items.length === 0 && <EmptyState message="You haven't played any casino rounds yet." />}

      {query.data && query.data.items.length > 0 && (
        <Card>
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-xs uppercase text-slate-500">
                <tr>
                  <th className="py-2 pr-4">Launched</th>
                  <th className="py-2 pr-4">Provider</th>
                  <th className="py-2 pr-4">Mode</th>
                  <th className="py-2 pr-4">Bet</th>
                  <th className="py-2 pr-4">Win</th>
                  <th className="py-2 pr-4">Rollback</th>
                  <th className="py-2 pr-4">Status</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {query.data.items.map((round) => (
                  <tr key={round.session_id}>
                    <td className="py-2 pr-4 text-slate-600">{formatLaunchedAt(round.launched_at)}</td>
                    <td className="py-2 pr-4">{round.provider_id}</td>
                    <td className="py-2 pr-4 capitalize">{round.mode}</td>
                    <td className="py-2 pr-4">{round.bet_amount !== undefined ? formatMoney(round.bet_amount, round.asset_code) : '-'}</td>
                    <td className="py-2 pr-4">{round.win_amount !== undefined ? formatMoney(round.win_amount, round.asset_code) : '-'}</td>
                    <td className="py-2 pr-4">
                      {round.rollback_amount !== undefined ? formatMoney(round.rollback_amount, round.asset_code) : '-'}
                    </td>
                    <td className="py-2 pr-4">
                      <Badge tone={statusTone(round.status)}>{round.status}</Badge>
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
