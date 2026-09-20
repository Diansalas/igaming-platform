import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export interface RgRestriction {
  id: string
  restriction_type: string
  scope: string
  starts_at: string
  ends_at?: string
  indefinite: boolean
  source: string
  reason_code?: string
  created_at: string
  active: boolean
}

/** Per-player restriction history - unchanged Stage 4D-RG shape, a bare array. */
export function listRestrictionsForPlayer(playerAccountId: string): Promise<RgRestriction[]> {
  return apiFetch(`/v1/admin/rg/restrictions${buildQuery({ player_account_id: playerAccountId })}`)
}

export interface ListRgRestrictionsParams {
  restriction_type?: 'self_exclusion'
  limit?: number
  offset?: number
}

/** Tenant-wide restriction queue - Stage 5 addition, paginated. */
export function listRgRestrictions(params: ListRgRestrictionsParams): Promise<PagedResponse<RgRestriction>> {
  return apiFetch(`/v1/admin/rg/restrictions${buildQuery(params)}`)
}
