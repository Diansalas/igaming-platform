import { NavLink } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { Button } from '../components/Button'
import { brandConfig } from '../config/brand'
import { visibleNavItems } from './nav'

export function TopBar() {
  const { isAuthenticated, logout } = useAuth()
  const items = visibleNavItems(isAuthenticated)

  return (
    <header className="flex h-14 shrink-0 items-center justify-between border-b border-border bg-surface px-4">
      <div className="flex items-center gap-6">
        <NavLink to="/" className="text-sm font-bold text-slate-900">
          {brandConfig.displayName}
        </NavLink>
        <nav className="hidden gap-1 sm:flex">
          {items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                `rounded-md px-3 py-2 text-sm font-medium ${
                  isActive ? 'bg-brand-50 text-brand-700' : 'text-slate-600 hover:bg-surface-alt'
                }`
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
      </div>
      <div>
        {isAuthenticated ? (
          <Button variant="secondary" onClick={logout}>
            Log out
          </Button>
        ) : (
          <NavLink to="/login">
            <Button>Log in</Button>
          </NavLink>
        )}
      </div>
    </header>
  )
}
