import { useAuth } from '../auth/AuthContext'
import { isPlatformAdmin } from '../auth/jwt'
import { Button } from '../components/Button'

export function TopBar() {
  const { claims, logout } = useAuth()
  if (!claims) return null

  const scopeLabel = isPlatformAdmin(claims) ? 'Platform admin' : `Tenant ${claims.tenant_id}`

  return (
    <header className="flex h-14 items-center justify-between border-b border-border bg-surface px-4">
      <div className="text-sm text-slate-500">{scopeLabel}</div>
      <div className="flex items-center gap-3 text-sm">
        <span className="text-slate-700">
          <span className="font-medium">{claims.role}</span>
        </span>
        <Button variant="secondary" onClick={logout}>
          Log out
        </Button>
      </div>
    </header>
  )
}
