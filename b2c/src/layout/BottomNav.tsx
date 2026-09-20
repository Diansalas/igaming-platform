import { NavLink } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { visibleNavItems } from './nav'

/** Mobile-only bottom navigation - a real sportsbook is used heavily on mobile, so primary navigation stays thumb-reachable rather than hidden behind a top-bar hamburger. */
export function BottomNav() {
  const { isAuthenticated } = useAuth()
  const items = visibleNavItems(isAuthenticated)

  return (
    <nav className="flex shrink-0 border-t border-border bg-surface sm:hidden">
      {items.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.to === '/'}
          className={({ isActive }) =>
            `flex-1 py-2 text-center text-xs font-medium ${isActive ? 'text-brand-700' : 'text-slate-500'}`
          }
        >
          {item.label}
        </NavLink>
      ))}
    </nav>
  )
}
