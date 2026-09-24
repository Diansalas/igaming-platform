import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export interface Tenant {
  id: string
  name: string
  slug: string
  licensing_model: string
  status: string
}

export interface Brand {
  id: string
  tenant_id: string
  name: string
  slug: string
  status: string
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

/** Every role createStaffRequest accepts (admin_routes.go's RequireOneOf). */
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
 * The subset a TENANT-scoped caller (tenant_admin) may create. finance,
 * risk_manager, promotions_manager and bonus_operations are refused with a
 * 403 by newCreateStaffHandler unless the caller is platform-scoped.
 */
export const TENANT_CREATABLE_STAFF_ROLES: readonly StaffRole[] = ['tenant_admin', 'support', 'compliance']

/** Mirrors the backend's minPasswordLen (internal/httpserver/auth_routes.go). */
export const MIN_STAFF_PASSWORD_LENGTH = 8

/** createStaffRequest - POST /v1/admin/tenants/{tenantID}/staff (PermStaffManage + canActOnTenant). */
export interface CreateStaffRequest {
  email: string
  password: string
  role: StaffRole
  person_id?: string
}

/** staffResponse. */
export interface StaffUser {
  id: string
  tenant_id?: string
  email: string
  role: string
  status: string
  person_id?: string
}

export function createStaff(tenantId: string, body: CreateStaffRequest): Promise<StaffUser> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/staff`, { method: 'POST', body: JSON.stringify(body) })
}

/** linkStaffPersonRequest - POST .../staff/{staffID}/person-link, 204 No Content on success. */
export function linkStaffPerson(tenantId: string, staffId: string, personId: string): Promise<void> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/staff/${encodeURIComponent(staffId)}/person-link`, {
    method: 'POST',
    body: JSON.stringify({ person_id: personId }),
  })
}
