// Decodes the payload of a platform-issued JWT (internal/auth/jwt.go's
// Claims) client-side, WITHOUT verifying its signature - a browser cannot
// verify an HMAC-signed token anyway (the signing key is server-only).
//
// THIS IS A UI CONVENIENCE ONLY, NEVER A SECURITY BOUNDARY. It exists
// purely so the app shell can show "who am I" and gate which nav items
// are rendered. The server independently and authoritatively verifies
// every token and every permission on every request; nothing derived
// from this decode is ever trusted as authorization. If this decode were
// tampered with or bypassed entirely, every protected endpoint would
// still correctly reject the caller server-side.
export interface StaffClaims {
  sub: string
  tenant_id: string
  role: string
  principal_type: string
  exp: number
  iat: number
  jti?: string
  iss?: string
  aud?: string[]
}

/** The all-zero UUID internal/auth uses to mark a platform-scoped (nil-tenant) principal. */
export const NIL_TENANT_ID = '00000000-0000-0000-0000-000000000000'

export function decodeJwtClaims(token: string): StaffClaims | null {
  try {
    const parts = token.split('.')
    if (parts.length !== 3) return null
    const base64 = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const padded = base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), '=')
    const binary = atob(padded)
    const bytes = Uint8Array.from(binary, (c) => c.charCodeAt(0))
    const json = new TextDecoder('utf-8').decode(bytes)
    const decoded = JSON.parse(json) as Partial<StaffClaims>
    if (!decoded.sub || !decoded.role || !decoded.tenant_id) return null
    return decoded as StaffClaims
  } catch {
    return null
  }
}

export function isPlatformAdmin(claims: StaffClaims | null): boolean {
  return !!claims && claims.tenant_id === NIL_TENANT_ID
}
