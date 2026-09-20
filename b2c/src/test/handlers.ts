import { http, HttpResponse } from 'msw'
import { makeTestJwt } from './jwt'

/**
 * Default, happy-path handlers for the endpoints this app's tests
 * exercise. Individual tests override a handler with `server.use(...)`
 * for a specific scenario (rejected bet, invalid credentials, etc.).
 */
export const handlers = [
  http.post('/v1/auth/login', async ({ request }) => {
    const body = (await request.json()) as { email: string; password: string; brand_slug?: string }
    if (body.email === 'player@example.com' && body.password === 'correct-password') {
      return HttpResponse.json({
        access_token: makeTestJwt(),
        refresh_token: 'refresh-token-1',
        token_type: 'Bearer',
        expires_in: 900,
      })
    }
    return HttpResponse.json({ code: 'unauthorized', message: 'invalid email or password' }, { status: 401 })
  }),

  http.post('/v1/auth/register', async ({ request }) => {
    const body = (await request.json()) as { email: string; password: string; brand_slug?: string }
    if (body.email === 'taken@example.com') {
      return HttpResponse.json({ code: 'conflict', message: 'an account with this email already exists' }, { status: 409 })
    }
    return HttpResponse.json(
      { access_token: makeTestJwt(), refresh_token: 'refresh-token-1', token_type: 'Bearer', expires_in: 900 },
      { status: 201 },
    )
  }),

  http.post('/v1/auth/refresh', () => {
    return HttpResponse.json({
      access_token: makeTestJwt(),
      refresh_token: 'refresh-token-2',
      token_type: 'Bearer',
      expires_in: 900,
    })
  }),

  http.get('/v1/me', () => {
    return HttpResponse.json({
      id: 'player-1',
      email: 'player@example.com',
      status: 'active',
      kyc_tier: 0,
      tenant_id: '11111111-1111-1111-1111-111111111111',
      brand_id: '22222222-2222-2222-2222-222222222222',
    })
  }),

  http.get('/v1/me/wallets', () => {
    return HttpResponse.json([
      {
        wallet_id: 'wallet-1',
        asset_code: 'USD',
        status: 'active',
        cash_balance: 10000,
        available_balance: 9500,
        held_for_withdrawal: 0,
        locked_balance: 500,
        bonus_balance: 0,
      },
    ])
  }),

  http.get('/v1/me/wallets/:assetCode', ({ params }) => {
    return HttpResponse.json({
      wallet_id: 'wallet-1',
      asset_code: params.assetCode,
      status: 'active',
      cash_balance: 10000,
      available_balance: 9500,
      held_for_withdrawal: 0,
      locked_balance: 500,
      bonus_balance: 0,
    })
  }),

  http.get('/v1/sportsbook/sports', () => {
    return HttpResponse.json([
      {
        id: 'sport-1',
        code: 'football',
        name: 'Football',
        competitions: [
          {
            id: 'comp-1',
            name: 'Premier League',
            events: [{ id: 'event-1', name: 'Home FC vs Away FC', start_time: '2026-01-01T18:00:00Z', status: 'open' }],
          },
        ],
      },
    ])
  }),

  http.get('/v1/sportsbook/events/:id', ({ params }) => {
    return HttpResponse.json({
      id: params.id,
      name: 'Home FC vs Away FC',
      start_time: '2026-01-01T18:00:00Z',
      status: 'open',
      sport_code: 'football',
      sport_name: 'Football',
      competition_name: 'Premier League',
      markets: [
        {
          id: 'market-1',
          name: 'Match Winner',
          status: 'open',
          selections: [
            { id: 'sel-1', name: 'Home FC', odds_numerator: 3, odds_denominator: 2, status: 'open' },
            { id: 'sel-2', name: 'Away FC', odds_numerator: 5, odds_denominator: 2, status: 'open' },
          ],
        },
      ],
    })
  }),

  http.post('/v1/me/sportsbook/bets', () => {
    return HttpResponse.json(
      {
        accepted: true,
        bet: {
          id: 'bet-1',
          selection_id: 'sel-1',
          asset_code: 'USD',
          stake_amount: 1000,
          odds_numerator: 3,
          odds_denominator: 2,
          potential_return: 2500,
          status: 'open',
          placed_at: '2026-01-01T00:00:00Z',
        },
      },
      { status: 201 },
    )
  }),

  http.get('/v1/me/sportsbook/bets', () => {
    return HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })
  }),

  http.get('/v1/me/deposits', () => {
    return HttpResponse.json([])
  }),

  http.get('/v1/me/casino/games', () => {
    return HttpResponse.json([
      {
        id: 'game-1',
        provider_id: 'mock-casino',
        name: 'Mock Slots Deluxe',
        game_type: 'slot',
        rtp_variant: 'standard',
        volatility: 'medium',
        feature_flags: [],
        supported_assets: ['USD'],
        mobile_supported: true,
        demo_supported: true,
      },
    ])
  }),

  http.post('/v1/me/casino/games/:gameId/launch', () => {
    return HttpResponse.json(
      { launch_url: 'https://mock-casino.example/launch/session-1', session_id: 'session-1', expires_at: '2026-01-01T01:00:00Z' },
      { status: 201 },
    )
  }),

  http.post('/v1/me/casino/sessions/:sessionId/wager', () => {
    return HttpResponse.json({
      outcome: 'succeeded',
      provider_tx_id: 'ptx-wager-1',
      ledger_transaction_id: 'ltx-wager-1',
      tombstoned: false,
    })
  }),

  http.post('/v1/me/casino/sessions/:sessionId/win', () => {
    return HttpResponse.json({
      outcome: 'succeeded',
      provider_tx_id: 'ptx-win-1',
      ledger_transaction_id: 'ltx-win-1',
      tombstoned: false,
    })
  }),

  http.post('/v1/me/casino/sessions/:sessionId/rollback', () => {
    return HttpResponse.json({
      outcome: 'succeeded',
      provider_tx_id: 'ptx-rollback-1',
      ledger_transaction_id: 'ltx-rollback-1',
      tombstoned: false,
    })
  }),

  http.get('/v1/me/casino/rounds', () => {
    return HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })
  }),
]
