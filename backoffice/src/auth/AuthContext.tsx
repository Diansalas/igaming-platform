import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useSyncExternalStore } from 'react'
import { staffLogin, staffLogout, type StaffLoginParams } from '../api/auth'
import { refreshSessionOnce } from '../api/client'
import { ApiError } from '../api/types'
import type { StaffClaims } from './jwt'
import { clearSession, getRefreshToken, getSessionSnapshot, setSession, subscribeSession } from './tokenStore'

interface AuthContextValue {
  claims: StaffClaims | null
  isAuthenticated: boolean
  /** True while the app is attempting to silently restore a session from the stored refresh token on first load. */
  isBootstrapping: boolean
  login: (params: StaffLoginParams) => Promise<void>
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const snapshot = useSyncExternalStore(subscribeSession, getSessionSnapshot, getSessionSnapshot)
  const [isBootstrapping, setIsBootstrapping] = useState(true)

  useEffect(() => {
    let cancelled = false
    const refreshToken = getRefreshToken()
    if (!refreshToken) {
      setIsBootstrapping(false)
      return
    }
    // Restore a session across a page reload: the access token lives only
    // in memory, so a reload always needs one silent refresh to get a
    // fresh one before we know who is signed in. Goes through the same
    // single-flight refreshSessionOnce() apiFetch uses (rather than a raw
    // refresh call) so this effect running twice under React 18
    // StrictMode - or racing a concurrent 401-triggered refresh - can
    // never present the same refresh token to the server twice, which the
    // backend treats as token reuse and revokes the whole session chain.
    refreshSessionOnce()
      .catch(() => {
        if (!cancelled) clearSession()
      })
      .finally(() => {
        if (!cancelled) setIsBootstrapping(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  const login = useCallback(async (params: StaffLoginParams) => {
    const pair = await staffLogin(params)
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
      login,
      logout,
    }),
    [snapshot, isBootstrapping, login, logout],
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
