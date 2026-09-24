import { http, HttpResponse } from 'msw'
import type { StaffClaims } from '../auth/jwt'
import { setSession } from '../auth/tokenStore'
import { makeTestJwt } from './jwt'
import { server } from './mswServer'

export const PLATFORM_ADMIN_CLAIMS: Partial<StaffClaims> = {
  role: 'platform_admin',
  tenant_id: '00000000-0000-0000-0000-000000000000',
  sub: 'platform-admin-1',
}

/**
 * Signs a test in as the given principal. Also pins /v1/auth/refresh to
 * the same token, since AuthProvider's bootstrap refresh would otherwise
 * replace it with the default handler's tenant_admin token.
 */
export function signInAs(claims: Partial<StaffClaims>): void {
  const token = makeTestJwt(claims)
  setSession(token, 'refresh-token-for-test')
  server.use(
    http.post('/v1/auth/refresh', () =>
      HttpResponse.json({ access_token: token, refresh_token: 'refresh-token-next', token_type: 'Bearer', expires_in: 900 }),
    ),
  )
}
