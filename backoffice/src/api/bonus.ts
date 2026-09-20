import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export type CampaignStatus = 'draft' | 'active' | 'paused' | 'ended' | 'archived'

export interface Campaign {
  id: string
  status: CampaignStatus
  fulfillment_owner: string
  brand_id?: string
  current_version_id?: string
  created_at?: string
}

export function listCampaigns(params: {
  status?: CampaignStatus
  limit?: number
  offset?: number
}): Promise<PagedResponse<Campaign>> {
  return apiFetch(`/v1/admin/bonus/campaigns${buildQuery(params)}`)
}

export type ChangeRequestStatus = 'pending' | 'applied' | 'rejected' | 'cancelled'

export interface ChangeRequest {
  id: string
  operation: string
  target_type: string
  target_id: string
  state: ChangeRequestStatus
  reason_code?: string
  requested_by_principal_id?: string
  requested_at?: string
  amount_at_request?: string
  asset_code?: string
  applied_by_principal_id?: string
  applied_at?: string
}

export function listChangeRequests(params: {
  status?: ChangeRequestStatus
  limit?: number
  offset?: number
}): Promise<PagedResponse<ChangeRequest>> {
  return apiFetch(`/v1/admin/bonus/change-requests${buildQuery(params)}`)
}

export function decideChangeRequest(
  id: string,
  decision: 'approve' | 'reject',
  reasonCode?: string,
): Promise<ChangeRequest> {
  return apiFetch(`/v1/admin/bonus/change-requests/${id}/decide`, {
    method: 'POST',
    body: JSON.stringify({ decision, reason_code: reasonCode }),
  })
}

export type GrantStatus =
  | 'issued'
  | 'activated'
  | 'in_progress'
  | 'pending_settlement'
  | 'completed'
  | 'converted'
  | 'expired'
  | 'cancelled'
  | 'forfeited'
  | 'reversed'

export interface Grant {
  id: string
  status: GrantStatus
  asset_code: string
  created_at: string
  activated_at?: string
  terminal_at?: string
  terminal_reason_code?: string
  remaining_bonus_balance?: string
}

export function listGrants(params: {
  player_account_id?: string
  status?: GrantStatus
  limit?: number
  offset?: number
}): Promise<PagedResponse<Grant>> {
  return apiFetch(`/v1/admin/bonus/grants${buildQuery(params)}`)
}
