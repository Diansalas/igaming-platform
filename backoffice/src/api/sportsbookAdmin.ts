import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export type AdminBetStatus = 'open' | 'settled_won' | 'settled_lost' | 'void'

export interface AdminBet {
  id: string
  player_account_id: string
  selection_id: string
  asset_code: string
  stake_amount: number
  odds_numerator: number
  odds_denominator: number
  potential_return: number
  status: AdminBetStatus
  placed_at: string
  decimal_exponent: number
}

export function listAdminSportsbookBets(params: { limit?: number; offset?: number }): Promise<PagedResponse<AdminBet>> {
  return apiFetch(`/v1/admin/sportsbook/bets${buildQuery(params)}`)
}
