import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { CasinoHistoryPage } from './CasinoHistoryPage'

describe('CasinoHistoryPage', () => {
  beforeEach(() => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
  })

  it('shows an empty state with no rounds', async () => {
    renderWithProviders(<CasinoHistoryPage />)
    expect(await screen.findByText("You haven't played any casino rounds yet.")).toBeInTheDocument()
  })

  it('renders rounds with the right status labels and amounts', async () => {
    server.use(
      http.get('/v1/me/casino/rounds', () =>
        HttpResponse.json({
          items: [
            {
              session_id: 'session-1',
              game_id: 'game-1',
              provider_id: 'mock-casino',
              provider_game_id: 'provider-game-1',
              asset_code: 'USD',
              mode: 'real',
              status: 'won',
              launched_at: '2026-01-01T00:00:00Z',
              bet_amount: 1000,
              win_amount: 2500,
              bet_provider_tx_id: 'ptx-wager-1',
              win_provider_tx_id: 'ptx-win-1',
            },
            {
              session_id: 'session-2',
              game_id: 'game-1',
              provider_id: 'mock-casino',
              provider_game_id: 'provider-game-1',
              asset_code: 'USD',
              mode: 'real',
              status: 'rolled_back',
              launched_at: '2026-01-02T00:00:00Z',
              bet_amount: 500,
              rollback_amount: 500,
              bet_provider_tx_id: 'ptx-wager-2',
              rollback_provider_tx_id: 'ptx-rollback-2',
            },
          ],
          limit: 20,
          offset: 0,
          total: 2,
        }),
      ),
    )

    renderWithProviders(<CasinoHistoryPage />)

    expect(await screen.findByText('won')).toBeInTheDocument()
    expect(screen.getByText('rolled_back')).toBeInTheDocument()
    expect(screen.getByText('10.00 USD')).toBeInTheDocument()
    expect(screen.getByText('25.00 USD')).toBeInTheDocument()
    expect(screen.getAllByText('5.00 USD')).toHaveLength(2)
  })
})
