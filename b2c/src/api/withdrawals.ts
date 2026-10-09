import { apiFetch } from './client'

// Mirrors internal/httpserver/withdrawal_handlers.go's
// withdrawalRequestResponse and internal/withdrawal's State values. A
// request places the amount on hold (held_for_withdrawal) and then waits
// for the operator's four-eyes review in the Back Office - nothing in this
// app can approve, submit or complete it.
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

export interface WithdrawalRequest {
  id: string
  asset_code: string
  amount: number
  state: WithdrawalState
  requested_at: string
  hold_ledger_transaction_id?: string
}

export function requestWithdrawal(params: {
  assetCode: string
  amount: number
  idempotencyKey: string
  /** Required by the backend (B13-B): the player's chosen payout instrument id. Re-validated server-side. */
  payoutInstrumentId: string
}): Promise<WithdrawalRequest> {
  return apiFetch<WithdrawalRequest>('/v1/me/withdrawals', {
    method: 'POST',
    body: JSON.stringify({
      asset_code: params.assetCode,
      amount: params.amount,
      idempotency_key: params.idempotencyKey,
      payout_instrument_id: params.payoutInstrumentId,
    }),
  })
}

export function listMyWithdrawals(): Promise<WithdrawalRequest[]> {
  return apiFetch<WithdrawalRequest[]>('/v1/me/withdrawals')
}

/** Only a `requested` withdrawal (not yet picked up for review) can be cancelled; otherwise 409. */
export function cancelWithdrawal(id: string): Promise<void> {
  return apiFetch<void>(`/v1/me/withdrawals/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
}
