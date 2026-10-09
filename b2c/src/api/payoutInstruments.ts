import { apiFetch } from './client'

// Mirrors internal/payoutinstrument.View as served by GET /v1/me/payout-instruments
// (internal/httpserver/payout_instrument_routes.go). MASKED data only: the
// backend never returns the destination detail, ciphertext, fingerprint or
// seal, and this app never asks for or stores them. `display_mask` is the
// only detail-derived value.
export type PayoutInstrumentState =
  | 'pending_verification'
  | 'verified'
  | 'verification_expired'
  | 'suspended'
  | 'revoked'
  | 'rejected'
  | 'superseded'

export interface PayoutInstrument {
  id: string
  kind: string
  rail: string
  asset_codes: string[]
  display_mask: string
  state: PayoutInstrumentState
  state_changed_at: string
  created_at: string
  verification_source?: string
  verified_at?: string
  verification_expires_at?: string
}

/** GET /v1/me/payout-instruments - the player's own instruments in every state (bare array, newest first). */
export function listMyPayoutInstruments(): Promise<PayoutInstrument[]> {
  return apiFetch<PayoutInstrument[]>('/v1/me/payout-instruments')
}

/**
 * Presentation filter ONLY. The list endpoint carries no `eligible` flag, so
 * the UI offers instruments that look usable for the chosen asset (verified,
 * listing the asset, verification not past its expiry). This is NOT an
 * authorization decision: POST /v1/me/withdrawals re-runs the full gate
 * (ownership, brand, verification seal, revoke/suspend, expiry, asset) and
 * its refusal is what the player is shown.
 */
export function isSelectableForAsset(inst: PayoutInstrument, assetCode: string, now: Date = new Date()): boolean {
  if (inst.state !== 'verified') return false
  if (!inst.asset_codes.includes(assetCode)) return false
  if (inst.verification_expires_at && new Date(inst.verification_expires_at).getTime() <= now.getTime()) return false
  return true
}
