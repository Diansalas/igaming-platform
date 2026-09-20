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
