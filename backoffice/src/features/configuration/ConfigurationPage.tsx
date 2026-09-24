import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { PageHeader } from '../../components/PageHeader'
import { CasinoCapabilityForm } from './CasinoCapabilityForm'
import { GameAvailabilityForm } from './GameAvailabilityForm'
import { PaymentCapabilityForm } from './PaymentCapabilityForm'

/** Tenant-scoped provider & game configuration (tenant_admin). Each form is gated by its own permission flag. */
export function ConfigurationPage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Providers & Games"
        description="Configure this tenant's payment and casino provider routing, and which catalogue games are available."
      />
      {perms.providerConfig && <PaymentCapabilityForm />}
      {perms.casinoConfig && <CasinoCapabilityForm />}
      {perms.casinoConfig && <GameAvailabilityForm />}
    </div>
  )
}
