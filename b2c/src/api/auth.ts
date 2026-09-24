import { apiFetch } from './client'

export interface TokenPairResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
}

/**
 * Player registration (internal/httpserver/auth_routes.go's
 * newRegisterHandler). `brandSlug` is what resolves which tenant/brand
 * this visitor belongs to server-side - it always comes from
 * `config/brand.ts`, never typed by the user or hardcoded per call site.
 */
export function register(params: { brandSlug: string; email: string; password: string }): Promise<TokenPairResponse> {
  return apiFetch<TokenPairResponse>('/v1/auth/register', {
    method: 'POST',
    body: JSON.stringify({ brand_slug: params.brandSlug, email: params.email, password: params.password }),
  })
}

export function login(params: { brandSlug: string; email: string; password: string }): Promise<TokenPairResponse> {
  return apiFetch<TokenPairResponse>('/v1/auth/login', {
    method: 'POST',
    body: JSON.stringify({ brand_slug: params.brandSlug, email: params.email, password: params.password }),
  })
}

/**
 * Revokes the session server-side (internal/auth.RevokeSessionByToken).
 * Uses `fetch` directly, not `apiFetch` - logout has no access token
 * worth attaching by the time it matters, and it must never trigger
 * apiFetch's own 401-refresh-retry logic. Best-effort: the caller clears
 * local session state regardless of whether this call succeeds, so a
 * network failure never traps the user in a "signed in" UI they can't
 * get out of.
 */
export function logout(refreshToken: string): Promise<void> {
  return fetch(`${import.meta.env.VITE_API_BASE_URL ?? ''}/v1/auth/logout`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
  }).then(() => undefined)
}

export interface MeResponse {
  id: string
  email: string
  status: string
  kyc_tier: number
  tenant_id: string
  brand_id: string
}

export function getMe(): Promise<MeResponse> {
  return apiFetch<MeResponse>('/v1/me')
}

/**
 * POST /v1/me/email-verification/request (internal/httpserver/
 * credential_handlers.go). Issues a verification code and sends it through
 * the platform's email provider. Normally 204 with no body. ONLY when the
 * server runs with the non-production account-activation test support
 * (APP_ENV!=production AND TEST_SUPPORT_ENDPOINTS_ENABLED=true) does it
 * answer 200 {"token": ...} - that decision is the server's alone, so the
 * UI shows a code only when one was actually returned and never has a
 * staging switch of its own. A rate-limited request is also a 204.
 */
export function requestEmailVerification(): Promise<{ token?: string }> {
  return apiFetch<{ token?: string } | undefined>('/v1/me/email-verification/request', { method: 'POST' }).then(
    (body) => body ?? {},
  )
}

/**
 * POST /v1/auth/email-verification/confirm - consumes the code and moves
 * the account from pending_verification to active. 204 on success; an
 * invalid, expired or already-used code is a 400 validation_error.
 */
export function confirmEmailVerification(token: string): Promise<void> {
  return apiFetch<void>('/v1/auth/email-verification/confirm', {
    method: 'POST',
    body: JSON.stringify({ token }),
  })
}
