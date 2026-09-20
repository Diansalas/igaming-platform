import { apiFetch } from './client'

export interface TokenPairResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
}

export interface StaffLoginParams {
  /** Omit entirely for a platform_admin login - every other role must supply it. */
  tenantSlug?: string
  email: string
  password: string
}

export function staffLogin(params: StaffLoginParams): Promise<TokenPairResponse> {
  const body: Record<string, string> = { email: params.email, password: params.password }
  if (params.tenantSlug) body.tenant_slug = params.tenantSlug
  return apiFetch<TokenPairResponse>('/v1/staff/auth/login', {
    method: 'POST',
    body: JSON.stringify(body),
  })
}

/**
 * Revokes the session server-side (internal/auth.RevokeSessionByToken).
 * Uses `fetch` directly, not `apiFetch` - logout has no access token to
 * attach by the time it matters (the caller clears local state right
 * after), and it must never trigger apiFetch's own 401-refresh-retry
 * logic. Best-effort: the caller clears local session state regardless
 * of whether this call succeeds, so a network failure never traps the
 * user in a "signed in" UI they can't get out of.
 */
export function staffLogout(refreshToken: string): Promise<void> {
  return fetch(`${import.meta.env.VITE_API_BASE_URL ?? ''}/v1/auth/logout`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: refreshToken }),
  }).then(() => undefined)
}
