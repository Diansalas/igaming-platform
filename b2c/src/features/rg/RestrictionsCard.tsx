import { useQuery } from '@tanstack/react-query'
import { getMyRgRestrictions } from '../../api/rg'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { formatWhen } from './rgSupport'

const TYPE_LABELS: Record<string, string> = { self_exclusion: 'Self-exclusion', time_out: 'Time-out' }

/**
 * The player's own restrictions exactly as the server reports them (GET /v1/me/rg/status). Whether a
 * restriction blocks play is decided server-side on every deposit, bet and launch; this card only shows it.
 */
export function RestrictionsCard() {
  const query = useQuery({ queryKey: ['rg-status'], queryFn: getMyRgRestrictions })
  const active = (query.data ?? []).filter((r) => r.active)
  const past = (query.data ?? []).filter((r) => !r.active)

  return (
    <Card title="Active restrictions">
      {query.isLoading && <LoadingSpinner label="Loading restrictions..." />}
      {query.error && <ErrorState error={query.error} onRetry={() => void query.refetch()} />}
      {query.data && active.length === 0 && <p className="text-sm text-slate-600">You have no active restrictions.</p>}
      {active.length > 0 && (
        <div className="flex flex-col gap-3">
          <p role="status" className="rounded-md border border-amber-200 bg-amber-50 p-3 text-sm text-amber-900">
            While a restriction is active you cannot deposit, place bets or launch games. You can still withdraw your funds.
          </p>
          <ul className="flex flex-col gap-2 text-sm">
            {active.map((r) => (
              <li key={r.id} className="flex flex-wrap items-center gap-2">
                <Badge tone="danger">{TYPE_LABELS[r.restriction_type] ?? r.restriction_type}</Badge>
                <span className="text-slate-700">
                  {r.indefinite ? 'No end date' : r.ends_at ? `Until ${formatWhen(r.ends_at)}` : ''}
                </span>
                <span className="text-xs text-slate-500">Applies to: {r.scope === 'platform' ? 'all brands' : r.scope}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
      {past.length > 0 && <p className="mt-3 text-xs text-slate-500">{past.length} earlier restriction(s) have ended.</p>}
    </Card>
  )
}
