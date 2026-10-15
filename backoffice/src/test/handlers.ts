import { http, HttpResponse } from 'msw'
import { makeTestJwt } from './jwt'

export const MFA_CHALLENGE_TOKEN = 'mfa-challenge-token-SENTINEL'
export const ENROL_CHALLENGE_TOKEN = 'enrol-challenge-token-SENTINEL'
export const ENROL_LINK_TOKEN = 'enrol-link-token-SENTINEL'
export const INVITE_LINK_TOKEN = 'invite-link-token-SENTINEL'
export const INVITE_PASSWORD = 'invite-password-SENTINEL-12'
export const VALID_TOTP = '123456'
export const VALID_RECOVERY_CODE = 'ABCDE-12345'
export const FACTOR_ID = '00000000-0000-4000-8000-0000000000f1'
export const TOTP_SECRET = 'JBSWY3DPEHPK3PXPSECRETSENTINEL'
export const RECOVERY_CODES = Array.from({ length: 10 }, (_, i) => `RCODE${i}-ABCD${i}`)

export function tokenPair(refresh: string) {
  return { access_token: makeTestJwt({ role: 'tenant_admin' }), refresh_token: refresh, token_type: 'Bearer', expires_in: 900 }
}

/**
 * Default, happy-path handlers for the endpoints this app's tests
 * exercise. Individual tests override a handler with `server.use(...)`
 * for a specific scenario (invalid credentials, lockout, etc.).
 */
export const handlers = [
  // ADR 0119 S3: password step yields an `mfa` challenge, never a token pair.
  http.post('/v1/staff/auth/login', async ({ request }) => {
    const body = (await request.json()) as { email: string; password: string; tenant_slug?: string }
    if (body.email === 'admin@example.com' && body.password === 'correct-password') {
      return HttpResponse.json({
        challenge: { type: 'mfa', token: MFA_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp', 'recovery_code'] },
      })
    }
    return HttpResponse.json({ code: 'unauthorized', message: 'invalid email or password' }, { status: 401 })
  }),

  http.post('/v1/staff/auth/mfa/verify', async ({ request }) => {
    const body = (await request.json()) as { challenge_token: string; code: string }
    if (body.challenge_token === MFA_CHALLENGE_TOKEN && body.code === VALID_TOTP) {
      return HttpResponse.json(tokenPair('refresh-token-1'))
    }
    return HttpResponse.json({ code: 'invalid_mfa_code', message: 'bad' }, { status: 401 })
  }),

  http.post('/v1/staff/auth/mfa/recover', async ({ request }) => {
    const body = (await request.json()) as { challenge_token: string; recovery_code: string }
    if (body.challenge_token === MFA_CHALLENGE_TOKEN && body.recovery_code === VALID_RECOVERY_CODE) {
      return HttpResponse.json({
        challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] },
      })
    }
    return HttpResponse.json({ code: 'invalid_mfa_code', message: 'bad' }, { status: 401 })
  }),

  http.post('/v1/staff/auth/enrol/redeem', async ({ request }) => {
    const body = (await request.json()) as { token: string; email: string; password: string }
    if (body.token === ENROL_LINK_TOKEN && body.email === 'admin@example.com' && body.password === 'correct-password') {
      return HttpResponse.json({
        challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] },
      })
    }
    return HttpResponse.json({ code: 'invalid_enrolment_token', message: 'bad' }, { status: 401 })
  }),

  // ADR 0119 S6: invitation acceptance (token-authenticated) yields an `enrol` challenge.
  http.post('/v1/staff/invitations/accept', async ({ request }) => {
    const body = (await request.json()) as { token: string; password: string }
    if (body.token === INVITE_LINK_TOKEN && body.password.length >= 12) {
      return HttpResponse.json({
        challenge: { type: 'enrol', token: ENROL_CHALLENGE_TOKEN, expires_in: 300, methods: ['totp'] },
      })
    }
    if (body.password.length < 12) return HttpResponse.json({ code: 'validation_error', message: 'bad' }, { status: 400 })
    return HttpResponse.json({ code: 'invalid_enrolment_token', message: 'bad' }, { status: 401 })
  }),

  http.post('/v1/staff/auth/enrol/start', async ({ request }) => {
    const body = (await request.json()) as { challenge_token: string }
    if (body.challenge_token !== ENROL_CHALLENGE_TOKEN) {
      return HttpResponse.json({ code: 'invalid_challenge', message: 'bad' }, { status: 401 })
    }
    return HttpResponse.json({
      factor_id: FACTOR_ID,
      otpauth_uri: `otpauth://totp/Test:admin@example.com?secret=${TOTP_SECRET}&issuer=Test&algorithm=SHA1&digits=6&period=30`,
      secret_base32: TOTP_SECRET,
      expires_in: 600,
    })
  }),

  http.post('/v1/staff/auth/enrol/confirm', async ({ request }) => {
    const body = (await request.json()) as { challenge_token: string; factor_id: string; code: string }
    if (body.challenge_token !== ENROL_CHALLENGE_TOKEN) {
      return HttpResponse.json({ code: 'invalid_challenge', message: 'bad' }, { status: 401 })
    }
    if (body.factor_id !== FACTOR_ID || body.code !== VALID_TOTP) {
      return HttpResponse.json({ code: 'invalid_mfa_code', message: 'bad' }, { status: 401 })
    }
    return HttpResponse.json({ recovery_codes: RECOVERY_CODES, token_pair: tokenPair('refresh-token-enrol') })
  }),

  http.get('/v1/staff/me/mfa', () =>
    HttpResponse.json({
      enrolled: true,
      factor_type: 'totp',
      activated_at: '2026-10-01T09:30:00Z',
      recovery_codes_remaining: 8,
      hard_locked: false,
      locked_until: null,
    }),
  ),

  http.post('/v1/auth/refresh', () => {
    return HttpResponse.json({
      access_token: makeTestJwt({ role: 'tenant_admin' }),
      refresh_token: 'refresh-token-2',
      token_type: 'Bearer',
      expires_in: 900,
    })
  }),

  // ADR 0112 launch panel reads (tenant/brand detail pages): empty by default, overridden per test.
  http.get('/v1/admin/tenants/:tid/launch-authorisations', () => HttpResponse.json({ items: [] })),
  http.get('/v1/admin/tenants/:tid/launch-status-history', () => HttpResponse.json({ items: [] })),

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
