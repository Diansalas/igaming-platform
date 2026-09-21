import { useQuery } from '@tanstack/react-query'
import { listGrants } from '../../api/bonus'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { formatUnscaledAmount } from '../../lib/money'
import { grantStatusTone } from './status'

/** Player-detail sub-section: this player's own bonus grants/reward state. */
export function PlayerBonusSummary({ playerAccountId }: { playerAccountId: string }) {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['bonus-grants', 'player', playerAccountId],
    queryFn: () => listGrants({ player_account_id: playerAccountId, limit: 20, offset: 0 }),
  })

  return (
    <Card title="Bonus grants">
      {isLoading ? (
        <LoadingSpinner />
      ) : error ? (
        <ErrorState error={error} onRetry={() => void refetch()} />
      ) : !data || data.items.length === 0 ? (
        <p className="text-sm text-slate-500">No bonus grants on file.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {data.items.map((g) => (
            <li key={g.id} className="flex items-center justify-between rounded-md border border-border px-3 py-2 text-sm">
              <span className="font-mono text-xs text-slate-500">{g.id}</span>
              {/* remaining_bonus_balance is a decimal-string minor-units
                  value with no decimal_exponent in the Grant API response -
                  see formatUnscaledAmount's doc comment. */}
              <span>{g.remaining_bonus_balance !== undefined ? formatUnscaledAmount(g.remaining_bonus_balance, g.asset_code) : '—'}</span>
              <Badge tone={grantStatusTone(g.status)}>{g.status}</Badge>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
