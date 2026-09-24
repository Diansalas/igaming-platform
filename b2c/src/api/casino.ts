import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

// Mirrors internal/httpserver's casino handlers exactly (field names/JSON
// tags). There is no real embeddable casino game client in this MVP slice
// - the "game/session screen" itself drives the financial simulation via
// the wager/win/rollback endpoints below. This module makes NO financial
// or eligibility decision of its own: every outcome (succeeded, declined,
// ambiguous, pending, or a thrown ApiError) is rendered exactly as the
// server returns it.

export interface CasinoGame {
  id: string
  provider_id: string
  name: string
  game_type: string
  rtp_variant?: string
  volatility?: string
  feature_flags: string[]
  supported_assets: string[]
  mobile_supported: boolean
  demo_supported: boolean
}

export function listCasinoGames(): Promise<CasinoGame[]> {
  return apiFetch<CasinoGame[]>('/v1/me/casino/games')
}

export type CasinoLaunchMode = 'real' | 'demo'

export interface LaunchCasinoGameParams {
  assetCode: string
  mode: CasinoLaunchMode
}

export interface CasinoLaunchResponse {
  launch_url: string
  session_id: string
  expires_at: string
}

export function launchCasinoGame(gameId: string, params: LaunchCasinoGameParams): Promise<CasinoLaunchResponse> {
  return apiFetch<CasinoLaunchResponse>(`/v1/me/casino/games/${encodeURIComponent(gameId)}/launch`, {
    method: 'POST',
    body: JSON.stringify({ asset_code: params.assetCode, mode: params.mode }),
  })
}

export type CasinoPlayOutcome = 'succeeded' | 'declined' | 'ambiguous' | 'pending'

/**
 * Shared response shape for wager/win/rollback. `outcome` is the
 * discriminator callers must branch on - a `declined` outcome is a normal,
 * fully-formed response (not a thrown error) and must be rendered as such,
 * matching the sportsbook bet slip's `accepted` convention.
 */
export interface CasinoPlayResult {
  outcome: CasinoPlayOutcome
  provider_tx_id: string
  ledger_transaction_id?: string
  tombstoned: boolean
  decline_reason?: string
}

// Wager and win REQUIRE idempotency_key (casino_play_handlers.go:
// RequireNonEmpty("idempotency_key")); without it the server answers 400.
// The caller mints one key per attempt and reuses it on retry.
export function placeCasinoWager(sessionId: string, stakeAmount: number, idempotencyKey: string): Promise<CasinoPlayResult> {
  return apiFetch<CasinoPlayResult>(`/v1/me/casino/sessions/${encodeURIComponent(sessionId)}/wager`, {
    method: 'POST',
    body: JSON.stringify({ stake_amount: stakeAmount, idempotency_key: idempotencyKey }),
  })
}

export function settleCasinoWin(sessionId: string, winAmount: number, idempotencyKey: string): Promise<CasinoPlayResult> {
  return apiFetch<CasinoPlayResult>(`/v1/me/casino/sessions/${encodeURIComponent(sessionId)}/win`, {
    method: 'POST',
    body: JSON.stringify({ win_amount: winAmount, idempotency_key: idempotencyKey }),
  })
}

export function rollbackCasinoPlay(sessionId: string, originalProviderTxId: string): Promise<CasinoPlayResult> {
  return apiFetch<CasinoPlayResult>(`/v1/me/casino/sessions/${encodeURIComponent(sessionId)}/rollback`, {
    method: 'POST',
    body: JSON.stringify({ original_provider_tx_id: originalProviderTxId }),
  })
}

export type CasinoRoundStatus = 'launched' | 'wagered' | 'won' | 'rolled_back'

export interface CasinoRound {
  session_id: string
  game_id: string
  provider_id: string
  provider_game_id: string
  asset_code: string
  mode: CasinoLaunchMode
  status: CasinoRoundStatus
  launched_at: string
  bet_amount?: number
  win_amount?: number
  rollback_amount?: number
  bet_provider_tx_id?: string
  win_provider_tx_id?: string
  rollback_provider_tx_id?: string
}

export function listCasinoRounds(params: { limit?: number; offset?: number }): Promise<PagedResponse<CasinoRound>> {
  return apiFetch<PagedResponse<CasinoRound>>(`/v1/me/casino/rounds${buildQuery(params)}`)
}
