import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export interface Tenant {
  id: string
  name: string
  slug: string
  licensing_model: string
  status: string
  /** List rows only: kind of the newest status transition; null when none. `legacy_baseline` marks a pre-governance subject. */
  launch_kind?: LaunchKind | null
}

export type LaunchKind = 'governed' | 'legacy_baseline' | 'owner_provisioned'

export interface Brand {
  id: string
  tenant_id: string
  name: string
  slug: string
  status: string
  /** List rows only: see Tenant.launch_kind. */
  launch_kind?: LaunchKind | null
}

export interface ListTenantsParams {
  q?: string
  status?: string
  limit?: number
  offset?: number
}

export function listTenants(params: ListTenantsParams): Promise<PagedResponse<Tenant>> {
  return apiFetch(`/v1/admin/tenants${buildQuery(params)}`)
}

export function getTenant(tenantId: string): Promise<Tenant> {
  return apiFetch(`/v1/admin/tenants/${tenantId}`)
}

export interface ListBrandsParams {
  limit?: number
  offset?: number
}

export function listBrands(tenantId: string, params: ListBrandsParams): Promise<PagedResponse<Brand>> {
  return apiFetch(`/v1/admin/tenants/${tenantId}/brands${buildQuery(params)}`)
}

export function getBrand(tenantId: string, brandId: string): Promise<Brand> {
  return apiFetch(`/v1/admin/tenants/${tenantId}/brands/${brandId}`)
}

// --- Provisioning writes (internal/httpserver/admin_routes.go) ---

export type LicensingModel = 'under_platform_licence' | 'own_licence'

/** createTenantRequest - POST /v1/admin/tenants (PermTenantWrite: platform_admin only). */
export interface CreateTenantRequest {
  name: string
  slug: string
  licensing_model: LicensingModel
  reason_code: string
}

export function createTenant(body: CreateTenantRequest): Promise<Tenant> {
  return apiFetch('/v1/admin/tenants', { method: 'POST', body: JSON.stringify(body) })
}

/** createBrandRequest - POST /v1/admin/tenants/{tenantID}/brands (PermBrandWrite + canActOnTenant). */
export interface CreateBrandRequest {
  name: string
  slug: string
}

export function createBrand(tenantId: string, body: CreateBrandRequest): Promise<Brand> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/brands`, { method: 'POST', body: JSON.stringify(body) })
}

/** Every role the invitation route accepts (StaffInvitationRequest.role). */
export const STAFF_ROLES = [
  'tenant_admin',
  'support',
  'compliance',
  'finance',
  'risk_manager',
  'promotions_manager',
  'bonus_operations',
] as const
export type StaffRole = (typeof STAFF_ROLES)[number]

/**
 * The subset a TENANT-scoped caller (tenant_admin) may invite (ADR 0119 contract section 1, "Role authority"); every
 * other role is `403 role_not_grantable` unless the caller is a platform admin. UI convenience only: the server decides.
 */
export const TENANT_GRANTABLE_STAFF_ROLES: readonly StaffRole[] = ['tenant_admin', 'support', 'compliance']

/** Roles that cannot be invited without a linked Person (contract section 2, "Invite"; otherwise `422 person_required`). */
export const PERSON_REQUIRED_STAFF_ROLES: readonly StaffRole[] = ['tenant_admin', 'finance', 'compliance']

/** Lifecycle status of a staff member (StaffDirectoryEntry.status). */
export type StaffStatus = 'invited' | 'active' | 'suspended' | 'offboarded'

/** StaffDirectoryEntry (contract section 6). `role` of a tenant list is always a tenant role. */
export interface StaffDirectoryEntry {
  id: string
  tenant_id?: string
  email: string
  display_name: string | null
  role: string
  status: StaffStatus
  mfa: { enrolled: boolean; hard_locked: boolean }
  person_linked: boolean
  last_login_at: string | null
  created_at: string
  status_changed_at: string | null
}

export interface StaffDirectoryList {
  staff: StaffDirectoryEntry[]
  /** True when more than 500 entries exist and the list was cut. */
  truncated: boolean
}

export interface ListStaffParams {
  status?: StaffStatus
  role?: StaffRole
}

/** GET /v1/admin/tenants/{tenantID}/staff - staff:read (platform_admin, tenant_admin, compliance). */
export function listTenantStaff(tenantId: string, params: ListStaffParams = {}): Promise<StaffDirectoryList> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/staff${buildQuery(params)}`)
}

/** StaffInvitationRequest - POST /v1/admin/tenants/{tenantID}/staff/invitations (staff:manage, step-up critical). */
export interface StaffInvitationRequest {
  email: string
  role: StaffRole
  person_id?: string
}

/** StaffLifecycleResult: never carries a token or credential; `email_delivery` says only whether the email was handed over. */
export interface StaffLifecycleResult {
  staff: StaffDirectoryEntry
  email_delivery?: 'sent' | 'failed'
}

export function inviteStaff(tenantId: string, body: StaffInvitationRequest): Promise<StaffLifecycleResult> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/staff/invitations`, {
    method: 'POST',
    body: JSON.stringify(body),
    cache: 'no-store',
  })
}

/** linkStaffPersonRequest - POST .../staff/{staffID}/person-link, 204 No Content on success. */
export function linkStaffPerson(tenantId: string, staffId: string, personId: string): Promise<void> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/staff/${encodeURIComponent(staffId)}/person-link`, {
    method: 'POST',
    body: JSON.stringify({ person_id: personId }),
  })
}
