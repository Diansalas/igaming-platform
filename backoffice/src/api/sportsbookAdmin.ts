import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export type AdminBetStatus = 'open' | 'settled_won' | 'settled_lost' | 'void'

/** `sportsbook_bet_settlements.event_kind` (docs/decisions/0088 §3.2). */
export type SettlementEventKind = 'settlement' | 'rollback' | 'void' | 'tombstone'

export type SettlementOutcome = 'won' | 'lost'

export type VoidReason = 'market_cancelled' | 'push' | 'data_error'

/**
 * One append-only `sportsbook_bet_settlements` row (docs/decisions/0088
 * §3.2/§3.4), read-only. Never rendered with any action control - this
 * app has no settle/void/rollback UI for W1 (the test-support simulation
 * route is staff-API-only and deliberately has no permission granted in
 * `auth/permissions.ts`).
 */
export interface SettlementLifecycleEvent {
  id: string
  event_kind: SettlementEventKind
  generation: number | null
  outcome: SettlementOutcome | null
  payout_amount: number | null
  void_reason: VoidReason | null
  ledger_transaction_id: string
  created_at: string
}

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
  /**
   * Stage 10 W1 (docs/decisions/0088 §3.4): read-only settlement fields,
   * absent on older cached/snapshot data - every reader treats these as
   * optional as well as nullable. The player-facing endpoint never
   * carries `correlation_id`/`lifecycle`/`actor_staff_account_id`/
   * `request_id`; those are admin-only.
   */
  outcome?: SettlementOutcome | null
  payout_amount?: number | null
  settled_at?: string | null
  correlation_id?: string
  lifecycle?: SettlementLifecycleEvent[]
}

export function listAdminSportsbookBets(params: { limit?: number; offset?: number }): Promise<PagedResponse<AdminBet>> {
  return apiFetch(`/v1/admin/sportsbook/bets${buildQuery(params)}`)
}
