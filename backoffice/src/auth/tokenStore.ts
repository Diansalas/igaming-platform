// Module-level session store, deliberately OUTSIDE React state, so the
// api/ layer (plain fetch calls, no React) and the auth/ React context can
// share the exact same source of truth for "what is the current session"
// without a circular import between them.
//
// Storage policy (ADR 0119 S7a, contract docs/plans/adr0119-s3-api-contract.md
// section 3): BOTH tokens live only in this in-memory module. Nothing is
// written to localStorage or sessionStorage, so a full page reload ends the
// session and the operator signs in again with a second factor (staff
// sessions are 12 h absolute / 30 min idle anyway). Before S7a the refresh
// token was kept in sessionStorage; that was removed because a staff refresh
// token now carries aal2 and the contract forbids browser storage for it.
//
// Fail closed: setSession refuses any access token that does not decode to
// an aal2 session (claim `aal` == 2). This is a UI convenience check on an
// unverified decode - the server is authoritative on every request - but it
// guarantees the UI never shows a signed-in state for a token the server
// did not mark as second-factor authenticated.
import { decodeJwtClaims, type StaffClaims } from './jwt'

let refreshToken: string | null = null

// One-time cleanup of the pre-S7a location of the refresh token (it is memory only now).
try {
  sessionStorage.removeItem('backoffice.refresh_token')
} catch {
  // Storage unavailable: nothing to clean.
}

interface SessionSnapshot {
  accessToken: string | null
  claims: StaffClaims | null
}

let snapshot: SessionSnapshot = { accessToken: null, claims: null }
const listeners = new Set<() => void>()

function emit() {
  for (const listener of listeners) listener()
}

export function subscribeSession(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function getSessionSnapshot(): SessionSnapshot {
  return snapshot
}

export function getAccessToken(): string | null {
  return snapshot.accessToken
}

export function getRefreshToken(): string | null {
  return refreshToken
}

export class InsufficientAssuranceError extends Error {
  constructor() {
    super('session is not a second-factor (aal2) session')
    this.name = 'InsufficientAssuranceError'
  }
}

export function setSession(accessToken: string, newRefreshToken: string): void {
  const claims = decodeJwtClaims(accessToken)
  if (!claims || claims.aal !== 2) {
    clearSession()
    throw new InsufficientAssuranceError()
  }
  snapshot = { accessToken, claims }
  refreshToken = newRefreshToken
  emit()
}

export function clearSession(): void {
  snapshot = { accessToken: null, claims: null }
  refreshToken = null
  emit()
}

/**
 * Step-up (ADR 0119 S4b/S7b): swaps ONLY the access token for the one returned by POST /v1/staff/auth/step-up.
 * The refresh token does not change. Fail closed: the new token must decode to an aal2 session of the SAME
 * subject as the current one; anything else ends the session and throws InsufficientAssuranceError. There is
 * no session to step up when signed out.
 */
export function replaceAccessToken(accessToken: string): void {
  const claims = decodeJwtClaims(accessToken)
  const current = snapshot.claims
  if (
    !current ||
    !claims || claims.aal !== 2 ||
    claims.sub !== current.sub ||
    claims.tenant_id !== current.tenant_id ||
    ((claims.sid !== undefined || current.sid !== undefined) && claims.sid !== current.sid)
  ) {
    clearSession()
    throw new InsufficientAssuranceError()
  }
  snapshot = { accessToken, claims }
  emit()
}
