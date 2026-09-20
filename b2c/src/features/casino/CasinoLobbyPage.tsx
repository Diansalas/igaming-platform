import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { launchCasinoGame, listCasinoGames, type CasinoGame } from '../../api/casino'
import { ApiError } from '../../api/types'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { brandConfig } from '../../config/brand'

function GameCard({
  game,
  isLaunching,
  launchError,
  onPlay,
}: {
  game: CasinoGame
  isLaunching: boolean
  launchError?: string
  onPlay: () => void
}) {
  return (
    <Card>
      <div className="flex flex-col gap-2">
        <h3 className="text-sm font-semibold text-slate-900">{game.name}</h3>
        <p className="text-xs capitalize text-slate-500">
          {game.game_type}
          {game.rtp_variant ? ` · ${game.rtp_variant}` : ''}
        </p>
        {game.volatility && <Badge tone="neutral">{game.volatility} volatility</Badge>}
        {launchError && (
          <p role="alert" className="text-xs text-red-700">
            {launchError}
          </p>
        )}
        <Button className="mt-2" isLoading={isLaunching} onClick={onPlay}>
          Play
        </Button>
      </div>
    </Card>
  )
}

/**
 * Flat grid of the tenant's casino catalogue - no search/filter/sort per
 * this stage's scope. "Play" launches a real-money session
 * (brandConfig.defaultAssetCode, mode "real") and navigates to the
 * session screen on success. A launch denial (403/404/etc) is rendered
 * inline on the card that triggered it, exactly as the server worded it -
 * this page makes no eligibility decision of its own.
 */
export function CasinoLobbyPage() {
  const navigate = useNavigate()
  const query = useQuery({ queryKey: ['casino', 'games'], queryFn: listCasinoGames })
  const [launchingId, setLaunchingId] = useState<string | null>(null)
  const [launchErrors, setLaunchErrors] = useState<Record<string, string>>({})

  async function onPlay(game: CasinoGame) {
    setLaunchingId(game.id)
    setLaunchErrors((prev) => ({ ...prev, [game.id]: '' }))
    try {
      const result = await launchCasinoGame(game.id, { assetCode: brandConfig.defaultAssetCode, mode: 'real' })
      navigate(`/casino/sessions/${result.session_id}`, { state: { launch: result, game } })
    } catch (err) {
      const message = err instanceof ApiError ? err.message : 'Could not launch this game. Please try again.'
      setLaunchErrors((prev) => ({ ...prev, [game.id]: message }))
    } finally {
      setLaunchingId(null)
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Casino" description="Pick a game to launch a session." />

      {query.isLoading && <LoadingSpinner label="Loading games..." />}
      {query.error && <ErrorState error={query.error} onRetry={() => void query.refetch()} />}
      {query.data && query.data.length === 0 && <EmptyState message="No games are available right now." />}

      {query.data && query.data.length > 0 && (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {query.data.map((game) => (
            <GameCard
              key={game.id}
              game={game}
              isLaunching={launchingId === game.id}
              launchError={launchErrors[game.id]}
              onPlay={() => void onPlay(game)}
            />
          ))}
        </div>
      )}
    </div>
  )
}
