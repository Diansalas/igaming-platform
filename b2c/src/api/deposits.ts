import { apiFetch } from './client'

// Mirrors internal/httpserver/deposit_handlers.go's depositIntentResponse.
// This is a real, existing, already-authorized development funding
// mechanism (internal/payments' mock provider) - never a shortcut that
// bypasses the ledger. A `pending` deposit is completed by the provider's
// signed webhook; outside production the server also exposes
// simulateDepositCallback below, which drives that same signed-webhook
// pipeline for the mock provider.
// Mirrors internal/payments DepositIntent* statuses.
export type DepositStatus = 'pending' | 'succeeded' | 'declined' | 'ambiguous' | 'failed'

export interface DepositIntent {
  id: string
  asset_code: string
  amount: number
  payment_method: string
  status: DepositStatus
  provider_id?: string
  redirect_url?: string
  hosted_field_token?: string
  ledger_transaction_id?: string
}

export function initiateDeposit(params: {
  assetCode: string
  amount: number
  paymentMethod: string
  idempotencyKey: string
}): Promise<DepositIntent> {
  return apiFetch<DepositIntent>('/v1/me/deposits', {
    method: 'POST',
    body: JSON.stringify({
      asset_code: params.assetCode,
      amount: params.amount,
      payment_method: params.paymentMethod,
      idempotency_key: params.idempotencyKey,
    }),
  })
}

export function listMyDeposits(): Promise<DepositIntent[]> {
  return apiFetch<DepositIntent[]>('/v1/me/deposits')
}

export function getMyDeposit(id: string): Promise<DepositIntent> {
  return apiFetch<DepositIntent>(`/v1/me/deposits/${id}`)
}

/**
 * POST /v1/me/deposits/{id}/simulate-callback (internal/httpserver/
 * payment_deposit_simulation_handlers.go). NON-PRODUCTION ONLY: the server
 * registers this route only when mock settlement is enabled (never in
 * production), and only for the player's own deposit routed to the mock
 * provider. It asks the mock provider to sign a normal "succeeded"
 * callback and feeds it through the same verify-and-post pipeline as the
 * public webhook - the ledger credit happens server-side exactly as for a
 * real PSP callback. When the route is not registered the server answers
 * a plain 404, which callers surface as "not available on this deployment".
 */
export interface SimulatedDepositCallbackResult {
  deposit_intent_id: string
  status: DepositStatus
  tombstoned: boolean
}

export function simulateDepositCallback(id: string): Promise<SimulatedDepositCallbackResult> {
  return apiFetch<SimulatedDepositCallbackResult>(`/v1/me/deposits/${encodeURIComponent(id)}/simulate-callback`, {
    method: 'POST',
  })
}
