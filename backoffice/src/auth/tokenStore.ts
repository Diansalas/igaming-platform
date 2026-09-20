// Module-level session store, deliberately OUTSIDE React state, so the
// api/ layer (plain fetch calls, no React) and the auth/ React context can
// share the exact same source of truth for "what is the current session"
// without a circular import between them.
//
// Storage policy (Stage 5 MVP, see the directive's own note): the ACCESS
// token lives only in this in-memory module variable - it disappears on a
// full page reload, which is fine because we always hold a refresh token
// to silently re-establish a session. The REFRESH token lives in
// sessionStorage (cleared when the tab closes). Production hardening would
// move both to httpOnly, Secure, SameSite cookies set by the server, which
// would remove the need for any of this client-side token handling at
// all - that requires backend changes out of scope for this stage.
import { decodeJwtClaims, type StaffClaims } from './jwt'

const REFRESH_TOKEN_KEY = 'backoffice.refresh_token'

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
  return sessionStorage.getItem(REFRESH_TOKEN_KEY)
}

export function setSession(accessToken: string, refreshToken: string): void {
  snapshot = { accessToken, claims: decodeJwtClaims(accessToken) }
  sessionStorage.setItem(REFRESH_TOKEN_KEY, refreshToken)
  emit()
}

export function clearSession(): void {
  snapshot = { accessToken: null, claims: null }
  sessionStorage.removeItem(REFRESH_TOKEN_KEY)
  emit()
}
