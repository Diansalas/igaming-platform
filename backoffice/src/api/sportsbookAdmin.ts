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
  /**
   * Stage 8 (docs/decisions/0080 Decision 5): passed through from
   * internal/sportsbook.Bet's own additive, always-empty-today
   * provider_id/provider_bet_reference pointer fields. Always an empty string,
   * never undefined, when this bet has no provider-acceptance reference
   * recorded - true for every bet today, since no real sportsbook
   * provider adapter exists yet.
   */
  provider_id: string
  provider_bet_reference: string
}

export function listAdminSportsbookBets(params: { limit?: number; offset?: number }): Promise<PagedResponse<AdminBet>> {
  return apiFetch(`/v1/admin/sportsbook/bets${buildQuery(params)}`)
}
