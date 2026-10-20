import { NavLink, Outlet } from 'react-router-dom'
import { PageHeader } from '../../components/PageHeader'
import { RegistryNote } from './LicensingBits'

/** Licensing and jurisdiction section: three tabs sharing one nav entry (platform_admin only; the server re-checks). */
export function LicensingLayout() {
  const tabClass = ({ isActive }: { isActive: boolean }) =>
    `rounded-md px-3 py-1.5 text-sm font-medium ${isActive ? 'bg-brand-50 text-brand-700' : 'text-slate-600 hover:bg-surface-alt'}`
  return (
    <div>
      <PageHeader title="Licensing and jurisdictions" description="Recorded configuration for jurisdictions, licences, tenant bindings and the changes that edit them." />
      <div className="mb-2 flex gap-1 border-b border-border pb-2">
        <NavLink to="/licensing/jurisdictions" className={tabClass}>
          Jurisdictions
        </NavLink>
        <NavLink to="/licensing/licences" className={tabClass}>
          Licences
        </NavLink>
        <NavLink to="/licensing/operating-bases" className={tabClass}>
          Operating bases
        </NavLink>
        <NavLink to="/licensing/changes" className={tabClass}>
          Licensing changes
        </NavLink>
      </div>
      <div className="mb-4">
        <RegistryNote />
      </div>
      <Outlet />
    </div>
  )
}
