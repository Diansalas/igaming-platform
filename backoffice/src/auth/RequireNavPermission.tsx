import type { ReactElement } from 'react'
import { NotAuthorized } from '../components/NotAuthorized'
import { useAuth } from './AuthContext'
import { getNavPermissions, type NavPermissions } from './permissions'

/**
 * Client-side section gate. Exactly like the nav itself, this is a
 * rendering convenience: reaching this component already means someone
 * typed or bookmarked a URL past a hidden nav item. If they hold the
 * real, server-side permission this only saves them a round trip to find
 * out the section doesn't apply to them; if they don't, every API call
 * the page underneath makes will still be independently rejected by the
 * server with a real 401/403 - this guard never substitutes for that.
 */
export function RequireNavPermission({
  permission,
  children,
}: {
  /** A single permission, or a list where holding ANY one is sufficient. */
  permission: keyof NavPermissions | (keyof NavPermissions)[]
  children: ReactElement
}) {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const required = Array.isArray(permission) ? permission : [permission]
  const allowed = required.some((p) => perms[p])
  if (!allowed) return <NotAuthorized />
  return children
}
