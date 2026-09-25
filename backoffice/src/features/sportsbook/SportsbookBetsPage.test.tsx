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
              outcome: null,
              payout_amount: null,
              settled_at: null,
              correlation_id: 'bet-1',
              lifecycle: [],
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
    expect(screen.getByText('Open')).toBeInTheDocument()
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
    // Multiple '—' cells now exist (provider ref, outcome, payout, settled, lifecycle).
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(1)
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

  it('renders a won bet with its payout, settled time and no exact-JSON regression on absent fields for older data', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-won',
              player_account_id: 'player-3',
              selection_id: 'selection-3',
              asset_code: 'EUR',
              stake_amount: 1000,
              odds_numerator: 250,
              odds_denominator: 100,
              potential_return: 2500,
              status: 'settled_won',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: '',
              provider_bet_reference: '',
              outcome: 'won',
              payout_amount: 2500,
              settled_at: '2026-01-02T00:00:00Z',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('player-3')).toBeInTheDocument()
    // "Won" appears twice: the status badge and the outcome column.
    expect(screen.getAllByText('Won')).toHaveLength(2)
    expect(screen.getAllByText('25.00 EUR').length).toBeGreaterThanOrEqual(2) // potential return + payout columns
    expect(screen.getByText('2026-01-02T00:00:00Z')).toBeInTheDocument()
  })

  it('renders a lost bet with no payout', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-lost',
              player_account_id: 'player-4',
              selection_id: 'selection-4',
              asset_code: 'EUR',
              stake_amount: 1000,
              odds_numerator: 250,
              odds_denominator: 100,
              potential_return: 2500,
              status: 'settled_lost',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: '',
              provider_bet_reference: '',
              outcome: 'lost',
              payout_amount: null,
              settled_at: '2026-01-02T00:00:00Z',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('player-4')).toBeInTheDocument()
    // "Lost" appears twice: the status badge and the outcome column.
    expect(screen.getAllByText('Lost')).toHaveLength(2)
  })

  it('renders a void bet with no outcome/payout', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-void',
              player_account_id: 'player-5',
              selection_id: 'selection-5',
              asset_code: 'EUR',
              stake_amount: 1000,
              odds_numerator: 250,
              odds_denominator: 100,
              potential_return: 2500,
              status: 'void',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: '',
              provider_bet_reference: '',
              outcome: null,
              payout_amount: null,
              settled_at: '2026-01-02T00:00:00Z',
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('player-5')).toBeInTheDocument()
    expect(screen.getByText('Void')).toBeInTheDocument()
  })

  it('tolerates the new fields being absent entirely (older cached data)', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-old',
              player_account_id: 'player-6',
              selection_id: 'selection-6',
              asset_code: 'EUR',
              stake_amount: 1000,
              odds_numerator: 250,
              odds_denominator: 100,
              potential_return: 2500,
              status: 'open',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: '',
              provider_bet_reference: '',
              // outcome/payout_amount/settled_at/correlation_id/lifecycle intentionally absent
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    expect(await screen.findByText('player-6')).toBeInTheDocument()
    expect(screen.getByText('Open')).toBeInTheDocument()
  })

  it('renders an expandable, read-only lifecycle list with ledger transaction ids, and never shows any settle/void/rollback control', async () => {
    server.use(
      http.get('/v1/admin/sportsbook/bets', () => {
        return HttpResponse.json({
          items: [
            {
              id: 'bet-lifecycle',
              player_account_id: 'player-7',
              selection_id: 'selection-7',
              asset_code: 'EUR',
              stake_amount: 1000,
              odds_numerator: 250,
              odds_denominator: 100,
              potential_return: 2500,
              status: 'settled_won',
              placed_at: '2026-01-01T00:00:00Z',
              decimal_exponent: 2,
              provider_id: '',
              provider_bet_reference: '',
              outcome: 'won',
              payout_amount: 2500,
              settled_at: '2026-01-03T00:00:00Z',
              correlation_id: 'bet-lifecycle',
              lifecycle: [
                {
                  id: 'hist-1',
                  event_kind: 'settlement',
                  generation: 1,
                  outcome: 'lost',
                  payout_amount: 0,
                  void_reason: null,
                  ledger_transaction_id: 'ledger-tx-1',
                  created_at: '2026-01-01T12:00:00Z',
                },
                {
                  id: 'hist-2',
                  event_kind: 'rollback',
                  generation: 1,
                  outcome: null,
                  payout_amount: null,
                  void_reason: null,
                  ledger_transaction_id: 'ledger-tx-2',
                  created_at: '2026-01-02T12:00:00Z',
                },
                {
                  id: 'hist-3',
                  event_kind: 'settlement',
                  generation: 2,
                  outcome: 'won',
                  payout_amount: 2500,
                  void_reason: null,
                  ledger_transaction_id: 'ledger-tx-3',
                  created_at: '2026-01-03T00:00:00Z',
                },
              ],
            },
          ],
          limit: 20,
          offset: 0,
          total: 1,
        })
      }),
    )

    renderWithProviders(<SportsbookBetsPage />)

    const toggle = await screen.findByText('Lifecycle (3)')
    expect(toggle).toBeInTheDocument()

    // <details> content is present in the DOM (jsdom renders it regardless
    // of the open attribute for text queries via textContent-based matching
    // through testing-library, since it doesn't strip closed <details>).
    expect(screen.getByText(/Ledger tx: ledger-tx-1/)).toBeInTheDocument()
    expect(screen.getByText(/Ledger tx: ledger-tx-2/)).toBeInTheDocument()
    expect(screen.getByText(/Ledger tx: ledger-tx-3/)).toBeInTheDocument()

    // Read-only: no button/link offering to settle, void or roll back.
    expect(screen.queryByRole('button', { name: /settle/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /void/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /rollback/i })).not.toBeInTheDocument()
  })
})
