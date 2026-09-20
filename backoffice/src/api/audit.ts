import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export interface AuditEntry {
  id: string
  actor_type: string
  actor_id?: string
  action: string
  target_type?: string
  target_id?: string
  outcome: string
  created_at: string
}

export interface ListAuditLogParams {
  actor_type?: string
  action?: string
  outcome?: string
  limit?: number
  offset?: number
}

export function listTenantAuditLog(params: ListAuditLogParams): Promise<PagedResponse<AuditEntry>> {
  return apiFetch(`/v1/admin/audit-log${buildQuery(params)}`)
}

export function listPlatformAuditLog(params: ListAuditLogParams): Promise<PagedResponse<AuditEntry>> {
  return apiFetch(`/v1/admin/platform/audit-log${buildQuery(params)}`)
}
