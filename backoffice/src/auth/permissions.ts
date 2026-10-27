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
  /**
   * This tenant's own detail + brand list (PermTenantRead + PermBrandRead on the own tenant):
   * tenant_admin, and since ADR 0112 slice 6 compliance (read only; brandWrite/staffAdmin stay false).
   */
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
  /** PermStaffManage - invite staff / link a staff person (POST /v1/admin/tenants/{tid}/staff/invitations, .../person-link). */
  staffAdmin: boolean
  /** PermStaffRead - the read-only staff directory (GET /v1/admin/tenants/{tid}/staff): platform_admin, tenant_admin, compliance. */
  staffRead: boolean
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
  /**
   * PRH-2 K3 (ADR 0101 24.9): PermPaymentForceResolveRead - view payment
   * force-resolutions (finance, compliance, platform_admin).
   */
  paymentForceResolutions: boolean
  /**
   * PRH-2 K3 (ADR 0101 24.9): PermPaymentForceResolveRequest/Approve - the
   * STATIC half only (finance, platform_admin; never tenant_admin). The
   * authority is an in-force payment_force_resolve capability GRANT read
   * server-side in the action's own transaction - this flag can never stand
   * in for it. ADR 0111 D-9 / S-6: the same capability also covers the
   * evidence-backed M4 resolutions (m4_evidence_paid / m4_evidence_not_paid),
   * which additionally need at least one platform_acting approver.
   */
  paymentForceResolutionAct: boolean
  /**
   * HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 6.3): PermWithdrawalHoldResolutionRead and
   * PermWithdrawalHoldResolutionRequest/Approve - the STATIC half only, platform_admin
   * ONLY (no tenant role, ever). The authority is an in-force platform grant plus a
   * G-P2 acting session, read server-side - this flag can never stand in for it.
   */
  withdrawalHoldResolutions: boolean
  withdrawalHoldResolutionAct: boolean
  /**
   * B13 (ADR 0111 2.9): PermPayoutInstrumentRead - view a player's payout
   * instruments (display mask only; finance, compliance, platform_admin).
   */
  payoutInstruments: boolean
  /**
   * B13 (ADR 0111 2.9): PermPayoutInstrumentSuspend - suspend an instrument
   * (compliance only). There is no staff create/verify/unsuspend/edit.
   */
  payoutInstrumentSuspend: boolean
  /**
   * ALERT-DELIVERY-1 (ADR 0102 section 18): PermAlertRouteManage - author alert
   * routing (who is paged). platform_admin only, and separate from the
   * ack/resolve permission. UI convenience only; the server re-checks.
   */
  alertRouting: boolean
  /**
   * ADR 0114 S1: PermAlertManage - list, acknowledge and resolve platform alerts and read routing
   * status. platform_admin ONLY (internal/auth/permission.go). UI convenience only; the server
   * re-checks every call and requires a platform-scope token.
   */
  alertManage: boolean
  /**
   * ADR 0112 (section 6.1): the static launch_authorisation:* governance permissions. UI
   * convenience only - the server re-checks every call (and enforces tenant scope: tenant
   * subjects are platform-only, a tenant role may act on its own tenant's BRANDS only).
   * read: platform_admin, tenant_admin, compliance. request and suspend: the same three.
   * approve: platform_admin ONLY (also the only role that sees the platform queue).
   */
  launchRead: boolean
  launchRequest: boolean
  launchApprove: boolean
  launchSuspend: boolean
  /** GET /v1/admin/launch-authorisations - platform staff only. */
  launchQueue: boolean
  /**
   * ADR 0114 S6: payments_kill_switch:read / :engage / :release - platform_admin and tenant_admin hold all
   * three (internal/auth/permission.go). One flag: the roles are identical. UI convenience only; the server
   * re-checks every call and a tenant caller may name only its own tenant.
   */
  killSwitch: boolean
  /**
   * ADR 0116 s6 (A7): the STATIC half of the AML officer gates (aml_case:read / :work / :close_approve, aml_hold:manage),
   * held by compliance and platform_admin only (internal/auth/permission.go). UI convenience only: the authority is the
   * in-force aml_officer capability grant, a signed proof and the officer quorum, all checked by the server.
   */
  amlRead: boolean
  amlWork: boolean
  amlApprove: boolean
  amlHolds: boolean
  /**
   * ADR 0114 S5: at least one reconciliation read permission (payment_statement / ledger_vs_projection:
   * platform_admin, compliance, finance; sportsbook_settlement and the casino streams: tenant_admin, compliance,
   * finance). Which STREAMS a role may open is `reconciliationStreams`. UI convenience only; the server checks
   * the permission of the requested stream before any query.
   */
  reconciliation: boolean
  /**
   * ADR 0114 S7 (K1, internal/auth/permission.go): capability_grant:read (platform_admin, tenant_admin,
   * compliance), :request (platform_admin, tenant_admin), :approve (platform_admin ONLY), :revoke (platform_admin,
   * tenant_admin). UI convenience only; the database decides R-7 and the distinct-person rules.
   */
  capabilityGrants: boolean
  capabilityGrantRequest: boolean
  capabilityGrantApprove: boolean
  capabilityGrantRevoke: boolean
  /**
   * lic5: jurisdiction_registry:read and licensing_change:read (jurisdictions, licences, ceilings, tenant jurisdiction
   * configuration and operating-country policies, the licensing change list). platform_admin ONLY. UI convenience only;
   * the server re-checks every call.
   */
  licensingRead: boolean
  /**
   * lic5: licensing_change:request plus the kinds' own permissions (jurisdiction_registry:manage, tenant_licence:assign) -
   * file a licence / binding / lifecycle change. platform_admin ONLY here (tenant staff file other kinds elsewhere).
   */
  licensingRequest: boolean
  /** lic5: licensing_change:approve - platform_admin ONLY (interim HDR-M-1). The database decides distinct Persons. */
  licensingApprove: boolean
  /**
   * lic16: kyc_enforcement_policy:read / :manage (lic10 L-20). platform_admin ONLY; a tenant token is refused. UI convenience only;
   * the database enforces that the activating administrator differs from the author.
   */
  kycPolicyRead: boolean
  kycPolicyManage: boolean
  /**
   * lic16: residence_approval_policy:read (tenant_admin, compliance; tenant-scoped sessions only, a platform token is refused) and
   * residence_approval_policy:manage plus verification:review (compliance only). residenceQueue mirrors verification:read for the
   * pending-determinations queue. UI convenience only.
   */
  residencePolicyRead: boolean
  residencePolicyManage: boolean
  residenceQueue: boolean
  /**
   * lic16: withdrawal_restriction_policy:read (platform_admin any tenant; tenant_admin and compliance their own) and
   * withdrawal_restriction_policy:change (platform_admin ONLY; also the only role that may read the change list).
   */
  wrpRead: boolean
  wrpChange: boolean
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
  staffRead: false,
  providerConfig: false,
  casinoConfig: false,
  catalogue: false,
  manualAdjustments: false,
  manualAdjustmentAct: false,
  paymentForceResolutions: false,
  paymentForceResolutionAct: false,
  withdrawalHoldResolutions: false,
  withdrawalHoldResolutionAct: false,
  payoutInstruments: false,
  payoutInstrumentSuspend: false,
  alertRouting: false,
  alertManage: false,
  launchRead: false,
  launchRequest: false,
  launchApprove: false,
  launchSuspend: false,
  launchQueue: false,
  killSwitch: false,
  amlRead: false,
  amlWork: false,
  amlApprove: false,
  amlHolds: false,
  reconciliation: false,
  capabilityGrants: false,
  capabilityGrantRequest: false,
  capabilityGrantApprove: false,
  capabilityGrantRevoke: false,
  licensingRead: false,
  licensingRequest: false,
  licensingApprove: false,
  kycPolicyRead: false,
  kycPolicyManage: false,
  residencePolicyRead: false,
  residencePolicyManage: false,
  residenceQueue: false,
  wrpRead: false,
  wrpChange: false,
}

// The write flags (tenantWrite/brandWrite/staffAdmin/providerConfig/
// casinoConfig/catalogue) mirror rolePermissions exactly:
//   PermTenantWrite           -> platform_admin
//   PermBrandWrite            -> platform_admin, tenant_admin
//   PermStaffManage           -> platform_admin, tenant_admin
//   PermProviderConfigWrite   -> tenant_admin (route also RequireTenantScope)
//   PermCasinoConfigWrite     -> tenant_admin (route also RequireTenantScope)
//   PermCasinoCatalogueManage -> platform_admin
// Which staff ROLES a staffAdmin caller may invite is a separate,
// service-level rule (finance/risk_manager/promotions_manager/
// bonus_operations need a platform admin; 403 role_not_grantable) - see
// api/tenants.ts's TENANT_GRANTABLE_STAFF_ROLES.
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
    staffRead: true,
    catalogue: true,
    manualAdjustments: true,
    manualAdjustmentAct: true,
    paymentForceResolutions: true,
    paymentForceResolutionAct: true,
    withdrawalHoldResolutions: true,
    withdrawalHoldResolutionAct: true,
    payoutInstruments: true,
    alertRouting: true,
    alertManage: true,
    launchRead: true,
    launchRequest: true,
    launchApprove: true,
    launchSuspend: true,
    launchQueue: true,
    killSwitch: true,
    amlRead: true,
    amlWork: true,
    amlApprove: true,
    amlHolds: true,
    reconciliation: true,
    capabilityGrants: true,
    capabilityGrantRequest: true,
    capabilityGrantApprove: true,
    capabilityGrantRevoke: true,
    licensingRead: true,
    licensingRequest: true,
    licensingApprove: true,
    kycPolicyRead: true,
    kycPolicyManage: true,
    wrpRead: true,
    wrpChange: true,
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
    staffRead: true,
    providerConfig: true,
    casinoConfig: true,
    launchRead: true,
    launchRequest: true,
    launchSuspend: true,
    killSwitch: true,
    reconciliation: true,
    capabilityGrants: true,
    capabilityGrantRequest: true,
    capabilityGrantRevoke: true,
    residencePolicyRead: true,
    residenceQueue: true,
    wrpRead: true,
  },
  support: { ...NONE, players: true, bonus: true, sportsbook: true, casino: true },
  compliance: { ...NONE, ownTenant: true, staffRead: true, players: true, kyc: true, rg: true, bonus: true, tenantAudit: true, sportsbook: true, casino: true, manualAdjustments: true, paymentForceResolutions: true, payoutInstruments: true, payoutInstrumentSuspend: true, launchRead: true, launchRequest: true, launchSuspend: true, amlRead: true, amlWork: true, amlApprove: true, amlHolds: true, reconciliation: true, capabilityGrants: true, residencePolicyRead: true, residencePolicyManage: true, residenceQueue: true, wrpRead: true },
  finance: {
    ...NONE,
    withdrawals: true,
    sportsbook: true,
    casino: true,
    manualAdjustments: true,
    manualAdjustmentAct: true,
    paymentForceResolutions: true,
    paymentForceResolutionAct: true,
    payoutInstruments: true,
    reconciliation: true,
  },
  risk_manager: { ...NONE },
  promotions_manager: { ...NONE, bonus: true },
  bonus_operations: { ...NONE, bonus: true },
}

export function getNavPermissions(role: string | undefined): NavPermissions {
  if (!role) return NONE
  return ROLE_NAV_PERMISSIONS[role] ?? NONE
}

export type ReconciliationStreamId = 'payment_statement' | 'ledger_vs_projection' | 'sportsbook_settlement' | 'casino_consistency' | 'casino_statement'

/**
 * Streams a role is expected to be allowed to read (ADR 0114 section 3 and 5, D-OPS-2). A static hint that only
 * decides which tabs to show; the server re-checks the stream permission on every call and a refusal is rendered.
 * Casino streams are tenant-scoped routes (no tenant in the path), so a platform principal gets none.
 */
export function reconciliationStreams(role: string | undefined, platform: boolean): ReconciliationStreamId[] {
  if (role === 'platform_admin') return platform ? ['payment_statement', 'ledger_vs_projection'] : []
  if (role === 'compliance' || role === 'finance') return ['payment_statement', 'ledger_vs_projection', 'sportsbook_settlement', 'casino_consistency', 'casino_statement']
  if (role === 'tenant_admin') return ['sportsbook_settlement', 'casino_consistency', 'casino_statement']
  return []
}
