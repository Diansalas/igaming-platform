import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { server } from '../../test/mswServer'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { CasinoRoundsPage } from './CasinoRoundsPage'

describe('CasinoRoundsPage', () => {
  beforeEach(() => {
    // Simulate an already-authenticated tenant_admin session so the page's
    // own API call carries a bearer token, matching how the app is
    // actually used (a signed-in operator navigating to /casino).
    setSession(makeTestJwt({ role: 'tenant_admin' }), 'refresh-token-for-test')
  })

  it('renders a row for each returned round, including a formatted status badge and money amounts', async () => {
    server.use(
      http.get('/v1/admin/casino/rounds', () => {
        return HttpResponse.json({
          items: [
            {
              session_id: 'session-1',
              game_id: 'game-1',
              provider_id: 'pragmatic',
              provider_game_id: 'vs20fruitsw',
              asset_code: 'USD',
              mode: 'real',
              status: 'won',
              launched_at: '2026-01-01T00:00:00Z',
              bet_amount: 1000,
              win_amount: 2500,
              player_account_id: 'player-1',
              brand_id: 'brand-1',
              decimal_exponent: 2,
              ledger_transaction_ids: ['ltx-1', 'ltx-2'],
              provider_round_id: 'ext-round-77',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<CasinoRoundsPage />)

    expect(await screen.findByText('player-1')).toBeInTheDocument()
    expect(screen.getByText('pragmatic/vs20fruitsw')).toBeInTheDocument()
    expect(screen.getByText('10.00 USD')).toBeInTheDocument()
    expect(screen.getByText('25.00 USD')).toBeInTheDocument()
    expect(screen.getByText('ext-round-77')).toBeInTheDocument()
    // No rollback on this round - rendered as a dash, never a raw undefined/blank.
    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.getByText('won')).toBeInTheDocument()
  })

  it('renders a dash for provider_round_id when no casino_provider_rounds binding exists yet', async () => {
    server.use(
      http.get('/v1/admin/casino/rounds', () => {
        return HttpResponse.json({
          items: [
            {
              session_id: 'session-2',
              game_id: 'game-2',
              provider_id: 'pragmatic',
              provider_game_id: 'vs20fruitsw',
              asset_code: 'USD',
              mode: 'real',
              status: 'launched',
              launched_at: '2026-01-01T00:00:00Z',
              player_account_id: 'player-2',
              brand_id: 'brand-1',
              decimal_exponent: 2,
              ledger_transaction_ids: [],
              provider_round_id: '',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<CasinoRoundsPage />)

    expect(await screen.findByText('player-2')).toBeInTheDocument()
    // Bet/win/rollback amounts AND provider_round_id are all unset here -
    // every one of them renders as a dash, never a raw empty string.
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(4)
  })

  it('shows the empty state when there are no casino rounds yet', async () => {
    server.use(
      http.get('/v1/admin/casino/rounds', () => {
        return HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })
      }),
    )

    renderWithProviders(<CasinoRoundsPage />)

    expect(await screen.findByText('No casino rounds yet.')).toBeInTheDocument()
  })
})
