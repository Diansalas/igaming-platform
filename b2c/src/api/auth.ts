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
