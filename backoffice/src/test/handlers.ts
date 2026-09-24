import { http, HttpResponse } from 'msw'
import { makeTestJwt } from './jwt'

/**
 * Default, happy-path handlers for the endpoints this app's tests
 * exercise. Individual tests override a handler with `server.use(...)`
 * for a specific scenario (invalid credentials, lockout, etc.).
 */
export const handlers = [
  http.post('/v1/staff/auth/login', async ({ request }) => {
    const body = (await request.json()) as { email: string; password: string; tenant_slug?: string }
    if (body.email === 'admin@example.com' && body.password === 'correct-password') {
      return HttpResponse.json({
        access_token: makeTestJwt({ role: 'tenant_admin' }),
        refresh_token: 'refresh-token-1',
        token_type: 'Bearer',
        expires_in: 900,
      })
    }
    return HttpResponse.json({ code: 'unauthorized', message: 'invalid email or password' }, { status: 401 })
  }),

  http.post('/v1/auth/refresh', () => {
    return HttpResponse.json({
      access_token: makeTestJwt({ role: 'tenant_admin' }),
      refresh_token: 'refresh-token-2',
      token_type: 'Bearer',
      expires_in: 900,
    })
  }),

  http.get('/v1/admin/withdrawals/history', () => {
    return HttpResponse.json({
      items: [
        {
          id: 'wd-1',
          asset_code: 'USD',
          amount: 10000,
          state: 'pending_review',
          requested_at: '2026-01-01T00:00:00Z',
          player_account_id: 'player-1',
        },
      ],
      limit: 20,
      offset: 0,
      total: 1,
    })
  }),

  // Pending-review queue (bare array). Real backend also promotes
  // requested -> pending_review as a side effect of this call.
  http.get('/v1/admin/withdrawals', () => {
    return HttpResponse.json([
      {
        id: 'wd-pending-1',
        asset_code: 'EUR',
        amount: 5000,
        state: 'pending_review',
        requested_at: '2026-01-02T00:00:00Z',
        player_account_id: 'player-2',
      },
    ])
  }),

  // Must be registered before '/v1/admin/withdrawals/:id', which would otherwise match it.
  http.get('/v1/admin/withdrawals/submitted', () => {
    return HttpResponse.json([
      {
        id: 'wd-submitted-1',
        asset_code: 'EUR',
        amount: 7000,
        state: 'submitted',
        requested_at: '2026-01-03T00:00:00Z',
        player_account_id: 'player-3',
      },
    ])
  }),

  http.get('/v1/admin/withdrawals/:id', ({ params }) => {
    return HttpResponse.json({
      id: params.id,
      asset_code: 'USD',
      amount: 10000,
      state: 'pending_review',
      requested_at: '2026-01-01T00:00:00Z',
      player_account_id: 'player-1',
    })
  }),

  http.post('/v1/admin/withdrawals/:id/approve', () => {
    return HttpResponse.json({ approved: true })
  }),
]
