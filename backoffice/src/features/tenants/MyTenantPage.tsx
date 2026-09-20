import { Navigate } from 'react-router-dom'
import { useAuth } from '../../auth/AuthContext'

/**
 * Tenant-scoped staff's "own tenant" nav entry - reuses the exact same
 * TenantDetailPage/BrandDetailPage routes and components a platform_admin
 * navigates to from the tenant list, just seeded with the caller's own
 * tenant_id from their JWT claims (a UI convenience for navigation only;
 * canActOnTenant is what actually authorizes the resulting GET server-side).
 */
export function MyTenantPage() {
  const { claims } = useAuth()
  if (!claims) return null
  return <Navigate to={`/tenants/${claims.tenant_id}`} replace />
}
