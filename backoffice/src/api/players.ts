import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export interface PlayerAccount {
  id: string
  brand_id: string
  email: string
  status: string
  kyc_tier: number
}

export interface ListPlayersParams {
  q?: string
  status?: string
  limit?: number
  offset?: number
}

export function listPlayers(params: ListPlayersParams): Promise<PagedResponse<PlayerAccount>> {
  return apiFetch(`/v1/admin/players${buildQuery(params)}`)
}

export function getPlayer(id: string): Promise<PlayerAccount> {
  return apiFetch(`/v1/admin/players/${id}`)
}

/** Body's `reason` field is required by the API (suspendPlayerRequest). */
export function suspendPlayer(id: string, reason: string): Promise<void> {
  return apiFetch(`/v1/admin/players/${id}/suspend`, {
    method: 'POST',
    body: JSON.stringify({ reason }),
  })
}

/** Body's `reason_code` field is required by the API (reinstatePlayerRequest). */
export function reinstatePlayer(id: string, reasonCode: string): Promise<void> {
  return apiFetch(`/v1/admin/players/${id}/reinstate`, {
    method: 'POST',
    body: JSON.stringify({ reason_code: reasonCode }),
  })
}
