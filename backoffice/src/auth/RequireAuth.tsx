import type { ReactElement } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { useAuth } from './AuthContext'

/**
 * Route guard: redirects an unauthenticated visitor to /login. This is a
 * client-side rendering convenience only - every API call this app makes
 * is independently authorized (or rejected) by the server regardless of
 * whether this guard ever ran.
 */
export function RequireAuth({ children }: { children: ReactElement }): ReactElement {
  const { isAuthenticated, isBootstrapping } = useAuth()
  const location = useLocation()

  if (isBootstrapping) {
    return (
      <div className="flex h-screen items-center justify-center">
        <LoadingSpinner label="Restoring your session..." />
      </div>
    )
  }

  if (!isAuthenticated) {
    return <Navigate to="/login" state={{ from: location }} replace />
  }

  return children
}
