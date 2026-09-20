// UI-ONLY convenience mirror of a SUBSET of internal/auth/permission.go's
// `rolePermissions` map - just enough to decide which nav sections/pages
// to render for the signed-in role. This is NEVER the security boundary:
// hiding a nav item does not stop a network call, and every protected
// endpoint independently and authoritatively re-checks the caller's real
// permission server-side (auth.RequirePermission). If a user reached a
// gated page some other way (e.g. typing the URL) and the API rejected
// the call, this app must show that rejection cleanly, never assume the
// nav gate was the real control.
//
// Kept intentionally narrow: only the flags Stage 5's pages actually need
// (players/kyc/rg/bonus/withdrawals/audit/tenants), not a full copy of
// every permission in the backend.
export interface NavPermissions {
  /** GET /v1/admin/tenants - the platform-wide tenant list (platform_admin only). */
  tenants: boolean
  /** This tenant's own detail + brand list (tenant_admin only). */
  ownTenant: boolean
  players: boolean
  kyc: boolean
  rg: boolean
  bonus: boolean
  withdrawals: boolean
  tenantAudit: boolean
  platformAudit: boolean
}

const NONE: NavPermissions = {
  tenants: false,
  ownTenant: false,
  players: false,
  kyc: false,
  rg: false,
  bonus: false,
  withdrawals: false,
  tenantAudit: false,
  platformAudit: false,
}

const ROLE_NAV_PERMISSIONS: Record<string, NavPermissions> = {
  platform_admin: { ...NONE, tenants: true, platformAudit: true },
  tenant_admin: { ...NONE, ownTenant: true, players: true, kyc: true, rg: true, bonus: true, tenantAudit: true },
  support: { ...NONE, players: true, bonus: true },
  compliance: { ...NONE, players: true, kyc: true, rg: true, bonus: true, tenantAudit: true },
  finance: { ...NONE, withdrawals: true },
  risk_manager: { ...NONE },
  promotions_manager: { ...NONE, bonus: true },
  bonus_operations: { ...NONE, bonus: true },
}

export function getNavPermissions(role: string | undefined): NavPermissions {
  if (!role) return NONE
  return ROLE_NAV_PERMISSIONS[role] ?? NONE
}
