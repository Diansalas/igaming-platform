import type { PlayerClaims } from '../auth/jwt'

/** Builds an unsigned-but-well-formed JWT string for tests (signature is not verified client-side). */
export function makeTestJwt(claims: Partial<PlayerClaims> = {}): string {
  const header = { alg: 'HS256', typ: 'JWT' }
  const payload: PlayerClaims = {
    sub: 'player-1',
    tenant_id: '11111111-1111-1111-1111-111111111111',
    role: 'player',
    principal_type: 'player',
    exp: Math.floor(Date.now() / 1000) + 3600,
    iat: Math.floor(Date.now() / 1000),
    ...claims,
  }
  const encode = (obj: unknown) => btoa(JSON.stringify(obj)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `${encode(header)}.${encode(payload)}.test-signature`
}
