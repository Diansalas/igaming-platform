import { useQuery } from '@tanstack/react-query'
import { listRestrictionsForPlayer } from '../../api/rg'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'

/** Player-detail sub-section: this player's own RG restriction history. */
export function PlayerRgSummary({ playerAccountId }: { playerAccountId: string }) {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['rg-restrictions', 'player', playerAccountId],
    queryFn: () => listRestrictionsForPlayer(playerAccountId),
  })

  return (
    <Card title="Responsible Gaming">
      {isLoading ? (
        <LoadingSpinner />
      ) : error ? (
        <ErrorState error={error} onRetry={() => void refetch()} />
      ) : !data || data.length === 0 ? (
        <p className="text-sm text-slate-500">No restrictions on file.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {data.map((r) => (
            <li key={r.id} className="flex items-center justify-between rounded-md border border-border px-3 py-2 text-sm">
              <span>
                {r.restriction_type} &middot; {r.scope}
              </span>
              <Badge tone={r.active ? 'warning' : 'neutral'}>{r.active ? 'Active' : 'Ended'}</Badge>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
