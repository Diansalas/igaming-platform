import { apiFetch, buildQuery } from './client'

// The player's responsible-gaming API (ADR 0115 S2/S4-min). Shapes mirror internal/httpserver
// rg_limits_player_handlers.go, rg_options_handlers.go and rg_handlers.go. This module is only a typed
// window: every decision (what is allowed, the effective limit, when an increase takes effect, whether a
// restriction blocks play) is made by the server; the UI never computes any of them.
//
// Every path used here is pinned against docs/api/openapi/platform-api.yaml by rg.contract.test.ts.

export type RgKind = 'deposit' | 'loss' | 'wager'
export type RgPeriod = 'rolling_24h' | 'rolling_7d' | 'rolling_30d' | 'calendar_day' | 'calendar_week' | 'calendar_month'

export interface RgRestriction {
  id: string
  restriction_type: 'self_exclusion' | 'time_out' | string
  scope: string
  starts_at: string
  ends_at?: string
  indefinite: boolean
  source: string
  reason_code?: string
  created_at: string
  active: boolean
}

/** GET /v1/me/rg/status - a bare array of the caller's own restrictions. */
export function getMyRgRestrictions(): Promise<RgRestriction[]> {
  return apiFetch<RgRestriction[]>('/v1/me/rg/status')
}

export interface RgLimitOption {
  kind: RgKind
  config_in_force: boolean
  mandatory: boolean
  allowed_periods: RgPeriod[]
  /** NUMERIC(38,0) minor units as a decimal string, or null (none). */
  ceiling: string | null
  scope?: 'account' | 'tenant_person'
  increase_cooldown_seconds: number | null
}

export interface RgOptions {
  asset_code: string
  limit_enforcement_enabled: boolean
  limits: RgLimitOption[]
  time_out_hours: number[]
  self_exclusion_days: number[]
  self_exclusion_indefinite_allowed: boolean
}

/** GET /v1/me/rg/options?asset_code= - the server-owned choices of this screen. */
export function getMyRgOptions(assetCode: string): Promise<RgOptions> {
  return apiFetch<RgOptions>(`/v1/me/rg/options${buildQuery({ asset_code: assetCode })}`)
}

export interface RgLimitChange {
  id: string
  kind: RgKind | string
  scope: string
  asset_code?: string
  period?: RgPeriod | string
  /** Decimal string of minor units; null = a removal. */
  value: string | null
  change_kind: 'initial' | 'decrease' | 'increase' | 'removal' | 'cancel_pending' | 'confirm' | string
  pending: boolean
  requires_confirmation: boolean
  requested_at: string
  effective_at: string
  supersedes_change_id?: string
  source: string
  reason_code: string
}

export interface RgLimitState {
  kind: RgKind | string
  scope: string
  asset_code?: string
  period?: RgPeriod | string
  has_value: boolean
  /** The REALIZED value of this key (what is enforced); null = none or removed. */
  value: string | null
  pending?: RgLimitChange
}

export interface RgMyLimits {
  limits: RgLimitState[]
  history: RgLimitChange[]
}

/** GET /v1/me/rg/limits - the realized value and the open pending change of every limit key. */
export function getMyRgLimits(): Promise<RgMyLimits> {
  return apiFetch<RgMyLimits>('/v1/me/rg/limits')
}

export interface SetRgLimitBody {
  kind: RgKind
  asset_code: string
  period: RgPeriod
  /** Decimal string of minor units; null removes the limit (a removal waits out the cooling-off period). */
  value: string | null
}

function idempotent(key: string): RequestInit['headers'] {
  return { 'Idempotency-Key': key }
}

/**
 * POST /v1/me/rg/limits. 201: effective now (initial value or decrease). 202: a pending increase / removal
 * - read effective_at (and requires_confirmation) from the returned change. The Idempotency-Key must be
 * held across a retry of the SAME attempt.
 */
export function setMyRgLimit(body: SetRgLimitBody, idempotencyKey: string): Promise<RgLimitChange> {
  return apiFetch<RgLimitChange>('/v1/me/rg/limits', {
    method: 'POST',
    headers: idempotent(idempotencyKey),
    body: JSON.stringify(body),
  })
}

/** POST /v1/me/rg/limits/changes/{id}/confirm - the player's own confirmation of an increase, after its cooling-off period. */
export function confirmMyRgLimitChange(changeId: string, idempotencyKey: string): Promise<RgLimitChange> {
  return apiFetch<RgLimitChange>(`/v1/me/rg/limits/changes/${encodeURIComponent(changeId)}/confirm`, {
    method: 'POST',
    headers: idempotent(idempotencyKey),
  })
}

/** POST /v1/me/rg/limits/changes/{id}/cancel - drop a pending increase / removal (protective, always allowed). */
export function cancelMyRgLimitChange(changeId: string, idempotencyKey: string): Promise<RgLimitChange> {
  return apiFetch<RgLimitChange>(`/v1/me/rg/limits/changes/${encodeURIComponent(changeId)}/cancel`, {
    method: 'POST',
    headers: idempotent(idempotencyKey),
  })
}

/** POST /v1/me/rg/time-out - a bounded break that cannot be ended early. Whole hours. */
export function createMyTimeOut(durationHours: number, idempotencyKey: string): Promise<RgRestriction> {
  return apiFetch<RgRestriction>('/v1/me/rg/time-out', {
    method: 'POST',
    headers: idempotent(idempotencyKey),
    body: JSON.stringify({ duration_hours: durationHours }),
  })
}

/** POST /v1/me/rg/self-exclusion - platform-wide, cannot be ended early. Omit durationDays for an indefinite exclusion. */
export function createMySelfExclusion(durationDays: number | null): Promise<RgRestriction> {
  return apiFetch<RgRestriction>('/v1/me/rg/self-exclusion', {
    method: 'POST',
    body: JSON.stringify(durationDays === null ? {} : { duration_days: durationDays }),
  })
}
