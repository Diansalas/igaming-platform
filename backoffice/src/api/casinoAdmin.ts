import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export type AdminRoundStatus = 'launched' | 'wagered' | 'won' | 'rolled_back'

export interface AdminRound {
  session_id: string
  game_id: string
  provider_id: string
  provider_game_id: string
  asset_code: string
  mode: 'real' | 'demo'
  status: AdminRoundStatus
  launched_at: string
  bet_amount?: number
  win_amount?: number
  rollback_amount?: number
  bet_provider_tx_id?: string
  win_provider_tx_id?: string
  rollback_provider_tx_id?: string
  player_account_id: string
  brand_id: string
  decimal_exponent: number
  ledger_transaction_ids: string[]
}

export function listAdminCasinoRounds(params: { limit?: number; offset?: number }): Promise<PagedResponse<AdminRound>> {
  return apiFetch(`/v1/admin/casino/rounds${buildQuery(params)}`)
}
