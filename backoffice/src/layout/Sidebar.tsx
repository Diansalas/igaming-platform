import { NavLink } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { buildNavItems } from './nav'

export function Sidebar() {
  const { claims } = useAuth()
  const items = buildNavItems(claims)

  return (
    <aside className="flex w-56 shrink-0 flex-col border-r border-border bg-surface">
      <div className="px-4 py-4 text-sm font-semibold text-slate-900">Back Office</div>
      <nav className="flex flex-col gap-0.5 px-2">
        {items.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
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
    </aside>
  )
}
