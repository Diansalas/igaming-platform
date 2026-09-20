import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { listSports, type CompetitionCatalogue, type EventSummary } from '../../api/sportsbook'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'

const EVENTS_PAGE_SIZE = 8

function eventStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'open':
    case 'scheduled':
      return 'success'
    case 'suspended':
      return 'warning'
    case 'closed':
    case 'settled':
      return 'neutral'
    default:
      return 'neutral'
  }
}

function formatStartTime(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  return date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

/**
 * `GET /v1/sportsbook/sports` returns the entire browse tree in one call
 * (no server-side pagination on the catalogue this stage) - to keep
 * discovery fast rather than a giant unpaginated dump if the catalogue
 * grows, each competition reveals its events incrementally client-side
 * ("Show more"), a presentation-only concern that never changes what the
 * server returned.
 */
function CompetitionSection({ competition }: { competition: CompetitionCatalogue }) {
  const [visibleCount, setVisibleCount] = useState(EVENTS_PAGE_SIZE)
  const visibleEvents = competition.events.slice(0, visibleCount)
  const remaining = competition.events.length - visibleEvents.length

  return (
    <details className="rounded-lg border border-border bg-surface" open={competition.events.length > 0}>
      <summary className="cursor-pointer select-none px-4 py-3 text-sm font-medium text-slate-800">
        {competition.name} <span className="font-normal text-slate-400">({competition.events.length})</span>
      </summary>
      {competition.events.length === 0 ? (
        <div className="px-4 pb-4">
          <EmptyState message="No events scheduled." />
        </div>
      ) : (
        <ul className="divide-y divide-border border-t border-border">
          {visibleEvents.map((event: EventSummary) => (
            <li key={event.id}>
              <Link
                to={`/sports/events/${event.id}`}
                className="flex items-center justify-between gap-3 px-4 py-3 text-sm hover:bg-surface-alt"
              >
                <div>
                  <p className="font-medium text-slate-900">{event.name}</p>
                  <p className="text-xs text-slate-500">{formatStartTime(event.start_time)}</p>
                </div>
                <Badge tone={eventStatusTone(event.status)}>{event.status}</Badge>
              </Link>
            </li>
          ))}
        </ul>
      )}
      {remaining > 0 && (
        <div className="border-t border-border px-4 py-2">
          <Button variant="ghost" onClick={() => setVisibleCount((v) => v + EVENTS_PAGE_SIZE)}>
            Show {Math.min(remaining, EVENTS_PAGE_SIZE)} more
          </Button>
        </div>
      )}
    </details>
  )
}

export function SportsListPage() {
  const query = useQuery({ queryKey: ['sportsbook', 'sports'], queryFn: listSports })

  return (
    <div>
      <PageHeader title="Sports" description="Browse events and add a selection to your bet slip." />

      {query.isLoading && <LoadingSpinner label="Loading sports..." />}
      {query.error && <ErrorState error={query.error} onRetry={() => void query.refetch()} />}
      {query.data && query.data.length === 0 && <EmptyState message="No sports are available right now." />}

      {query.data && query.data.length > 0 && (
        <div className="flex flex-col gap-6">
          {query.data.map((sport) => (
            <section key={sport.id}>
              <h2 className="mb-2 text-sm font-semibold uppercase tracking-wide text-slate-500">{sport.name}</h2>
              <div className="flex flex-col gap-2">
                {sport.competitions.length === 0 ? (
                  <EmptyState message="No competitions available." />
                ) : (
                  sport.competitions.map((competition) => <CompetitionSection key={competition.id} competition={competition} />)
                )}
              </div>
            </section>
          ))}
        </div>
      )}
    </div>
  )
}
