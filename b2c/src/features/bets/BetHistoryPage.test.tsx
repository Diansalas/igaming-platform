import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { BetHistoryPage } from './BetHistoryPage'

/**
 * docs/decisions/0088 §3.4: this page is read-only. These tests only
 * assert what is rendered - never that a settle/void/rollback action is
 * available, because no such control exists in this UI.
 */
describe('BetHistoryPage', () => {
  it('renders an open bet with no payout/settled time', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          items: [
            {
              id: 'bet-open',
              selection_id: 'sel-1',
              asset_code: 'USD',
              stake_amount: 1000,
              odds_numerator: 3,
              odds_denominator: 2,
              potential_return: 2500,
              status: 'open',
              placed_at: '2026-01-01T00:00:00Z',
              outcome: null,
              payout_amount: null,
              settled_at: null,
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        }),
      ),
    )

    renderWithProviders(<BetHistoryPage />)

    expect(await screen.findByText('Open')).toBeInTheDocument()
    const dashes = screen.getAllByText('—')
    expect(dashes.length).toBeGreaterThanOrEqual(2) // payout column + settled column
  })

  it('renders a won bet with its payout and settled time', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          items: [
            {
              id: 'bet-won',
              selection_id: 'sel-1',
              asset_code: 'USD',
              stake_amount: 1000,
              odds_numerator: 3,
              odds_denominator: 2,
              potential_return: 2500,
              status: 'settled_won',
              placed_at: '2026-01-01T00:00:00Z',
              outcome: 'won',
              payout_amount: 2500,
              settled_at: '2026-01-02T00:00:00Z',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        }),
      ),
    )

    renderWithProviders(<BetHistoryPage />)

    expect(await screen.findByText('Won')).toBeInTheDocument()
    // Payout column and Potential return column both render 25.00 USD - assert at least one instance.
    expect(screen.getAllByText('25.00 USD').length).toBeGreaterThanOrEqual(2)
  })

  it('renders a lost bet with no payout', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          items: [
            {
              id: 'bet-lost',
              selection_id: 'sel-1',
              asset_code: 'USD',
              stake_amount: 1000,
              odds_numerator: 3,
              odds_denominator: 2,
              potential_return: 2500,
              status: 'settled_lost',
              placed_at: '2026-01-01T00:00:00Z',
              outcome: 'lost',
              payout_amount: null,
              settled_at: '2026-01-02T00:00:00Z',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        }),
      ),
    )

    renderWithProviders(<BetHistoryPage />)

    expect(await screen.findByText('Lost')).toBeInTheDocument()
    expect(screen.getByText('—')).toBeInTheDocument() // payout dash
  })

  it('renders a void bet with no outcome/payout', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          items: [
            {
              id: 'bet-void',
              selection_id: 'sel-1',
              asset_code: 'USD',
              stake_amount: 1000,
              odds_numerator: 3,
              odds_denominator: 2,
              potential_return: 2500,
              status: 'void',
              placed_at: '2026-01-01T00:00:00Z',
              outcome: null,
              payout_amount: null,
              settled_at: '2026-01-02T00:00:00Z',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        }),
      ),
    )

    renderWithProviders(<BetHistoryPage />)

    expect(await screen.findByText('Void')).toBeInTheDocument()
    expect(screen.getByText('—')).toBeInTheDocument() // payout dash
  })

  it('tolerates older data with the new fields absent entirely', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(
      http.get('/v1/me/sportsbook/bets', () =>
        HttpResponse.json({
          items: [
            {
              id: 'bet-old',
              selection_id: 'sel-1',
              asset_code: 'USD',
              stake_amount: 1000,
              odds_numerator: 3,
              odds_denominator: 2,
              potential_return: 2500,
              status: 'open',
              placed_at: '2026-01-01T00:00:00Z',
              // outcome/payout_amount/settled_at intentionally absent
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        }),
      ),
    )

    renderWithProviders(<BetHistoryPage />)

    expect(await screen.findByText('Open')).toBeInTheDocument()
  })
})
