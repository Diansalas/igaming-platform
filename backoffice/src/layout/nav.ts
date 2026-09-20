import { getNavPermissions } from '../auth/permissions'
import { isPlatformAdmin, type StaffClaims } from '../auth/jwt'

export interface NavItem {
  label: string
  to: string
}

/**
 * Builds the sidebar's nav items for the signed-in principal. Platform
 * admin and tenant-scoped staff are two different login experiences (see
 * the Stage 5 directive) - this is where that split becomes concrete
 * navigation, not a tenant switcher that has nothing behind it.
 */
export function buildNavItems(claims: StaffClaims | null): NavItem[] {
  if (!claims) return []
  const perms = getNavPermissions(claims.role)
  const items: NavItem[] = []

  if (isPlatformAdmin(claims)) {
    if (perms.tenants) items.push({ label: 'Tenants', to: '/tenants' })
    if (perms.platformAudit) items.push({ label: 'Platform Audit Log', to: '/platform-audit-log' })
    return items
  }

  if (perms.ownTenant) items.push({ label: 'My Tenant', to: '/my-tenant' })
  if (perms.players) items.push({ label: 'Players', to: '/players' })
  if (perms.kyc) items.push({ label: 'KYC', to: '/kyc' })
  if (perms.rg) items.push({ label: 'Responsible Gaming', to: '/rg' })
  if (perms.bonus) items.push({ label: 'Bonus', to: '/bonus/campaigns' })
  if (perms.withdrawals) items.push({ label: 'Withdrawals', to: '/withdrawals' })
  if (perms.sportsbook) items.push({ label: 'Sportsbook', to: '/sportsbook' })
  if (perms.tenantAudit) items.push({ label: 'Audit Log', to: '/audit-log' })

  return items
}
