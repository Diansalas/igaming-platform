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
  /**
   * Stage 8 (docs/decisions/0080 Decision 5): the casino_provider_rounds
   * binding for this round's own launch session, when one exists. Always
   * an empty string, never undefined, when no binding exists yet (a
   * launched-but-never-wagered round, or - before a real provider adapter
   * exists - the general case for historical data predating this field).
   */
  provider_round_id: string
}

export function listAdminCasinoRounds(params: { limit?: number; offset?: number }): Promise<PagedResponse<AdminRound>> {
  return apiFetch(`/v1/admin/casino/rounds${buildQuery(params)}`)
}

// --- Casino configuration writes (internal/httpserver/casino_admin_handlers.go) ---

/**
 * writeCasinoCapabilityRequest - PUT /v1/admin/casino/providers/{providerID}/capability
 * (PermCasinoConfigWrite + RequireTenantScope: tenant_admin only). Narrowing-only.
 */
export interface CasinoCapabilityRequest {
  supports_catalogue: boolean
  supports_launch: boolean
  supports_balance: boolean
  supports_bet: boolean
  supports_win: boolean
  supports_rollback: boolean
  supported_assets: string[]
  supported_game_types: string[]
  priority: number
  status: 'active' | 'disabled'
}

export function writeCasinoCapability(providerId: string, body: CasinoCapabilityRequest): Promise<{ id: string }> {
  return apiFetch(`/v1/admin/casino/providers/${encodeURIComponent(providerId)}/capability`, {
    method: 'PUT',
    body: JSON.stringify(body),
  })
}

/**
 * upsertCasinoGameRequest - PUT /v1/admin/casino/games
 * (PermCasinoCatalogueManage: platform_admin only, NOT tenant-scoped).
 */
export interface UpsertCasinoGameRequest {
  provider_id: string
  provider_game_id: string
  name: string
  game_type: string
  rtp_variant?: string
  volatility?: string
  feature_flags?: string[]
  supported_assets?: string[]
  mobile_supported: boolean
  demo_supported: boolean
  jurisdiction_blocklist?: string[]
  status?: 'active' | 'disabled'
}

/** casinoGameResponse (internal/httpserver/casino_handlers.go). */
export interface CasinoGame {
  id: string
  provider_id: string
  name: string
  game_type: string
  rtp_variant?: string
  volatility?: string
  feature_flags: string[] | null
  supported_assets: string[] | null
  mobile_supported: boolean
  demo_supported: boolean
}

export function upsertCasinoGame(body: UpsertCasinoGameRequest): Promise<CasinoGame> {
  return apiFetch('/v1/admin/casino/games', { method: 'PUT', body: JSON.stringify(body) })
}

/**
 * setCasinoGameAvailabilityRequest - PUT /v1/admin/casino/games/{gameID}/availability
 * (PermCasinoConfigWrite + RequireTenantScope: tenant_admin only).
 */
export function setCasinoGameAvailability(gameId: string, enabled: boolean): Promise<{ game_id: string; enabled: boolean }> {
  return apiFetch(`/v1/admin/casino/games/${encodeURIComponent(gameId)}/availability`, {
    method: 'PUT',
    body: JSON.stringify({ enabled }),
  })
}
