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
  /** Stage 6: tenant-wide sportsbook bet visibility (PermSportsbookBetRead). */
  sportsbook: boolean
  /** Stage 7: tenant-wide casino round visibility (PermCasinoTransactionRead). */
  casino: boolean
  /** PermTenantWrite - create a tenant (POST /v1/admin/tenants). platform_admin only. */
  tenantWrite: boolean
  /** PermBrandWrite - create a brand (POST /v1/admin/tenants/{tid}/brands). */
  brandWrite: boolean
  /** PermStaffManage - create staff / link a staff person (POST /v1/admin/tenants/{tid}/staff...). */
  staffAdmin: boolean
  /** PermProviderConfigWrite - payment provider capability (tenant-scoped). */
  providerConfig: boolean
  /** PermCasinoConfigWrite - casino provider capability + game availability (tenant-scoped). */
  casinoConfig: boolean
  /** PermCasinoCatalogueManage - platform-wide casino catalogue upsert. platform_admin only. */
  catalogue: boolean
  /**
   * PRH-2 K2 (ADR 0100): PermLedgerAdjustmentRead - view governed manual
   * adjustment requests (finance, compliance, platform_admin).
   */
  manualAdjustments: boolean
  /**
   * PRH-2 K2 (ADR 0100): PermLedgerAdjustmentInitiate/Approve - the STATIC
   * half only (finance, platform_admin). The authority is an in-force
   * ledger_adjustment capability GRANT read server-side in the action's own
   * transaction (and, for platform_admin, a G-P2 grant for the tenant) -
   * this flag can never stand in for it.
   */
  manualAdjustmentAct: boolean
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
  sportsbook: false,
  casino: false,
  tenantWrite: false,
  brandWrite: false,
  staffAdmin: false,
  providerConfig: false,
  casinoConfig: false,
  catalogue: false,
  manualAdjustments: false,
  manualAdjustmentAct: false,
}

// The write flags (tenantWrite/brandWrite/staffAdmin/providerConfig/
// casinoConfig/catalogue) mirror rolePermissions exactly:
//   PermTenantWrite           -> platform_admin
//   PermBrandWrite            -> platform_admin, tenant_admin
//   PermStaffManage           -> platform_admin, tenant_admin
//   PermProviderConfigWrite   -> tenant_admin (route also RequireTenantScope)
//   PermCasinoConfigWrite     -> tenant_admin (route also RequireTenantScope)
//   PermCasinoCatalogueManage -> platform_admin
// Which staff ROLES a staffAdmin caller may create is a separate,
// handler-level rule (finance/risk_manager/promotions_manager/
// bonus_operations need a platform-scoped caller) - see
// api/tenants.ts's TENANT_CREATABLE_STAFF_ROLES.
//
// Stage 6's sportsbook flag mirrors exactly which roles the backend grants
// PermSportsbookBetRead to (internal/auth/permission.go): tenant_admin,
// compliance, support, finance - never platform_admin, since platform_admin
// has no tenant context to view bets in (db.Pool.WithTenant hard-errors on
// uuid.Nil). Stage 7's casino flag mirrors PermCasinoTransactionRead the
// same way, for the identical reason and the identical role set: a player
// always sees their own casino history via the self-service endpoint (RLS/
// explicit ownership checks, not RBAC, authorize that), and platform_admin
// still has no tenant-scoped player_account to resolve.
const ROLE_NAV_PERMISSIONS: Record<string, NavPermissions> = {
  platform_admin: {
    ...NONE,
    tenants: true,
    platformAudit: true,
    tenantWrite: true,
    brandWrite: true,
    staffAdmin: true,
    catalogue: true,
    manualAdjustments: true,
    manualAdjustmentAct: true,
  },
  tenant_admin: {
    ...NONE,
    ownTenant: true,
    players: true,
    kyc: true,
    rg: true,
    bonus: true,
    tenantAudit: true,
    sportsbook: true,
    casino: true,
    brandWrite: true,
    staffAdmin: true,
    providerConfig: true,
    casinoConfig: true,
  },
  support: { ...NONE, players: true, bonus: true, sportsbook: true, casino: true },
  compliance: { ...NONE, players: true, kyc: true, rg: true, bonus: true, tenantAudit: true, sportsbook: true, casino: true, manualAdjustments: true },
  finance: { ...NONE, withdrawals: true, sportsbook: true, casino: true, manualAdjustments: true, manualAdjustmentAct: true },
  risk_manager: { ...NONE },
  promotions_manager: { ...NONE, bonus: true },
  bonus_operations: { ...NONE, bonus: true },
}

export function getNavPermissions(role: string | undefined): NavPermissions {
  if (!role) return NONE
  return ROLE_NAV_PERMISSIONS[role] ?? NONE
}
