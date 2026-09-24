import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export type WithdrawalState =
  | 'requested'
  | 'pending_review'
  | 'approved'
  | 'rejected'
  | 'submitted'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'reversed'

export interface Withdrawal {
  id: string
  asset_code: string
  amount: number
  state: WithdrawalState
  requested_at: string
  hold_ledger_transaction_id?: string
  player_account_id: string
  /**
   * Minor-unit exponent for `asset_code` (e.g. 2 for EUR meaning `amount`
   * is in cents), populated by the admin history/detail endpoints only.
   * Absent (not 0) means "unknown" - never assume 0 when this is missing,
   * render the raw minor-units value with a visible "(exponent unknown)"
   * qualifier instead of guessing. A ledger-finance review flagged
   * rendering raw minor units with no exponent as a real financial-clarity
   * defect at an irreversible approval decision point.
   */
  decimal_exponent?: number
}

export function listWithdrawals(params: {
  status?: WithdrawalState
  limit?: number
  offset?: number
}): Promise<PagedResponse<Withdrawal>> {
  return apiFetch(`/v1/admin/withdrawals/history${buildQuery(params)}`)
}

export function getWithdrawal(id: string): Promise<Withdrawal> {
  return apiFetch(`/v1/admin/withdrawals/${id}`)
}

export function approveWithdrawal(id: string): Promise<{ approved: boolean }> {
  return apiFetch(`/v1/admin/withdrawals/${id}/approve`, { method: 'POST' })
}

/** `reason_code` is required by the API. */
export function rejectWithdrawal(id: string, reasonCode: string): Promise<void> {
  return apiFetch(`/v1/admin/withdrawals/${id}/reject`, {
    method: 'POST',
    body: JSON.stringify({ reason_code: reasonCode }),
  })
}

/**
 * GET /v1/admin/withdrawals - the four-eyes PENDING-REVIEW queue (bare
 * array, not paged). SIDE EFFECT: the backend first promotes every
 * `requested` withdrawal in this tenant to `pending_review` (closing the
 * player's cancellation window) before listing. Only call this from the
 * explicit "Pending review" queue view.
 */
export function listPendingWithdrawals(): Promise<Withdrawal[]> {
  return apiFetch('/v1/admin/withdrawals')
}

/** GET /v1/admin/withdrawals/submitted - withdrawals at `submitted` awaiting resolution (bare array). */
export function listSubmittedWithdrawals(): Promise<Withdrawal[]> {
  return apiFetch('/v1/admin/withdrawals/submitted')
}

/** submitWithdrawalResponse / resolve response: the withdrawal plus the provider it went to. */
export interface SubmittedWithdrawal {
  id: string
  asset_code: string
  amount: number
  state: WithdrawalState
  requested_at: string
  hold_ledger_transaction_id?: string
  provider_id?: string
  provider_reference?: string
}

/** POST /v1/admin/withdrawals/{id}/submit {payment_method} - only valid on an `approved` withdrawal. */
export function submitWithdrawal(id: string, paymentMethod: string): Promise<SubmittedWithdrawal> {
  return apiFetch(`/v1/admin/withdrawals/${id}/submit`, {
    method: 'POST',
    body: JSON.stringify({ payment_method: paymentMethod }),
  })
}

/** POST /v1/admin/withdrawals/{id}/resolve (no body) - queries the recorded provider for a `submitted` withdrawal. */
export function resolveWithdrawal(id: string): Promise<SubmittedWithdrawal> {
  return apiFetch(`/v1/admin/withdrawals/${id}/resolve`, { method: 'POST' })
}
