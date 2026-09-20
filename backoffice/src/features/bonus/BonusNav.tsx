import { NavLink, Outlet } from 'react-router-dom'

/** Two related bonus sub-pages sharing one nav entry - simple in-page tabs, not two sidebar items. */
export function BonusLayout() {
  const tabClass = ({ isActive }: { isActive: boolean }) =>
    `rounded-md px-3 py-1.5 text-sm font-medium ${isActive ? 'bg-brand-50 text-brand-700' : 'text-slate-600 hover:bg-surface-alt'}`

  return (
    <div>
      <div className="mb-4 flex gap-1 border-b border-border pb-2">
        <NavLink to="/bonus/campaigns" className={tabClass}>
          Campaigns
        </NavLink>
        <NavLink to="/bonus/change-requests" className={tabClass}>
          Change requests
        </NavLink>
      </div>
      <Outlet />
    </div>
  )
}
