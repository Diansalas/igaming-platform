import { useQueryClient } from '@tanstack/react-query'
import { createContext, useCallback, useContext, useEffect, useMemo, type ReactNode } from 'react'
import { useSyncExternalStore } from 'react'
import { staffLogout, type TokenPairResponse } from '../api/auth'
import { ApiError } from '../api/types'
import type { StaffClaims } from './jwt'
import { clearSession, getRefreshToken, getSessionSnapshot, setSession, subscribeSession } from './tokenStore'

interface AuthContextValue {
  claims: StaffClaims | null
  isAuthenticated: boolean
  /** Always false since S7a: tokens are memory only, so there is no session to restore on load (kept for RequireAuth). */
  isBootstrapping: boolean
  /**
   * Installs the token pair returned by the second factor (mfa/verify or
   * enrol/confirm). Throws InsufficientAssuranceError - and stays signed out -
   * if the access token is not an aal2 session.
   */
  establishSession: (pair: TokenPairResponse) => void
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const snapshot = useSyncExternalStore(subscribeSession, getSessionSnapshot, getSessionSnapshot)
  const queryClient = useQueryClient()
  const isBootstrapping = false

  // Memory-only tokens (ADR 0119 S7a): there is no stored refresh token to restore a session from after a
  // reload, so there is nothing to bootstrap. When the session ends, drop every cached server response.
  useEffect(() => {
    if (snapshot.claims === null) queryClient.clear()
  }, [snapshot.claims, queryClient])

  const establishSession = useCallback((pair: TokenPairResponse) => {
    setSession(pair.access_token, pair.refresh_token)
  }, [])

  const logout = useCallback(() => {
    const refreshToken = getRefreshToken()
    // Best-effort server-side revocation (internal/auth.RevokeSessionByToken)
    // - without this, "signing out" only cleared local state while the
    // refresh token remained valid server-side for its full TTL. Local
    // state is cleared regardless of whether the server call succeeds, so
    // a network failure never traps the user in a signed-in UI.
    if (refreshToken) {
      staffLogout(refreshToken).catch(() => {
        // Nothing actionable client-side; the user is signed out locally
        // either way.
      })
    }
    clearSession()
  }, [])

  const value = useMemo<AuthContextValue>(
    () => ({
      claims: snapshot.claims,
      isAuthenticated: snapshot.claims !== null,
      isBootstrapping,
      establishSession,
      logout,
    }),
    [snapshot, isBootstrapping, establishSession, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within an AuthProvider')
  return ctx
}

/** Re-exported so callers that just need "was this an auth failure" don't need to import from api/. */
export { ApiError }
