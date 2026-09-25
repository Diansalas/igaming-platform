import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from 'react-router-dom'
import { getEvent, type MarketDetail, type Selection } from '../../api/sportsbook'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { useBetSlip } from '../betslip/BetSlipContext'
import { formatDecimalOdds } from '../../lib/odds'

function statusTone(status: string): BadgeTone {
  switch (status) {
    case 'open':
    case 'scheduled':
      return 'success'
    case 'suspended':
      return 'warning'
    default:
      return 'neutral'
  }
}

function formatStartTime(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

function MarketCard({ eventId, eventName, market }: { eventId: string; eventName: string; market: MarketDetail }) {
  const { selection: current, setSelection } = useBetSlip()

  function onPick(sel: Selection) {
    setSelection({
      selectionId: sel.id,
      selectionName: sel.name,
      marketName: market.name,
      eventId,
      eventName,
      oddsNumerator: sel.odds_numerator,
      oddsDenominator: sel.odds_denominator,
    })
  }

  return (
    <Card
      title={
        <span className="flex items-center gap-2">
          {market.name}
          <Badge tone={statusTone(market.status)}>{market.status}</Badge>
        </span>
      }
    >
      {market.selections.length === 0 ? (
        <p className="text-sm text-slate-500">No selections in this market.</p>
      ) : (
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
          {market.selections.map((sel) => {
            const isSelected = current?.selectionId === sel.id
            // Markets are `open`/`suspended`/`closed`, but a selection's status is
            // `active`/`suspended` (internal/sportsbook/types.go SelectionStatus).
            const isPickable = market.status === 'open' && sel.status === 'active'
            return (
              <button
                key={sel.id}
                type="button"
                disabled={!isPickable}
                onClick={() => onPick(sel)}
                className={`flex flex-col items-center gap-1 rounded-md border px-3 py-2 text-sm transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                  isSelected ? 'border-brand-600 bg-brand-50' : 'border-border bg-white hover:bg-surface-alt'
                }`}
              >
                <span className="font-medium text-slate-800">{sel.name}</span>
                <span className="font-semibold text-brand-700">{formatDecimalOdds(sel.odds_numerator, sel.odds_denominator)}</span>
              </button>
            )
          })}
        </div>
      )}
    </Card>
  )
}

export function EventDetailPage() {
  const { id = '' } = useParams()
  const query = useQuery({ queryKey: ['sportsbook', 'event', id], queryFn: () => getEvent(id), enabled: !!id })

  if (query.isLoading) return <LoadingSpinner label="Loading event..." />
  if (query.error) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  const event = query.data
  if (!event) return null

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={event.name}
        description={`${event.sport_name} - ${event.competition_name} - ${formatStartTime(event.start_time)}`}
        actions={<Badge tone={statusTone(event.status)}>{event.status}</Badge>}
      />

      <Link to="/sports" className="text-sm text-brand-700 hover:underline">
        &larr; Back to sports
      </Link>

      {event.markets.length === 0 ? (
        <p className="text-sm text-slate-500">No markets are available for this event yet.</p>
      ) : (
        <div className="flex flex-col gap-4">
          {event.markets.map((market) => (
            <MarketCard key={market.id} eventId={event.id} eventName={event.name} market={market} />
          ))}
        </div>
      )}
    </div>
  )
}
