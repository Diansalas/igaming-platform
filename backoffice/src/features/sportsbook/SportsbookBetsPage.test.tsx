import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { server } from '../../test/mswServer'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { SportsbookBetsPage } from './SportsbookBetsPage'

describe('SportsbookBetsPage', () => {
  beforeEach(() => {
    // Simulate an already-authenticated tenant_admin session so the page's
    // own API call carries a bearer token, matching how the app is
    // actually used (a signed-in operator navigating to /sportsbook).
    setSession(makeTestJwt({ role: 'tenant_admin' }), 'refresh-token-for-test')
  })

  it('renders a row for each returned bet, including the provider reference when one is set', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-1',
              player_account_id: 'player-1',
              selection_id: 'selection-1',
              asset_code: 'EUR',
              stake_amount: 1000,
              odds_numerator: 250,
              odds_denominator: 100,
              potential_return: 2500,
              status: 'open',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: 'dummy-sportsbook',
              provider_bet_reference: 'dummy-ref-42',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('player-1')).toBeInTheDocument()
    expect(screen.getByText('10.00 EUR')).toBeInTheDocument()
    expect(screen.getByText('2.50')).toBeInTheDocument()
    expect(screen.getByText('dummy-sportsbook/dummy-ref-42')).toBeInTheDocument()
    expect(screen.getByText('open')).toBeInTheDocument()
  })

  it('renders a dash for the provider reference when both provider_id and provider_bet_reference are empty', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-2',
              player_account_id: 'player-2',
              selection_id: 'selection-2',
              asset_code: 'EUR',
              stake_amount: 500,
              odds_numerator: 200,
              odds_denominator: 100,
              potential_return: 1000,
              status: 'open',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: '',
              provider_bet_reference: '',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('player-2')).toBeInTheDocument()
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('shows the empty state when there are no sportsbook bets yet', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('No sportsbook bets placed yet.')).toBeInTheDocument()
  })
})
