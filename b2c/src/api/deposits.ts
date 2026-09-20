import { apiFetch } from './client'

// Mirrors internal/httpserver/deposit_handlers.go's depositIntentResponse.
// This is a real, existing, already-authorized development funding
// mechanism (internal/payments' mock provider) - never a shortcut that
// bypasses the ledger. See DepositPage.tsx's own doc comment for the one
// disclosed gap this app cannot work around: the mock provider's webhook
// callback is HMAC-signed with a secret generated server-side and never
// exposed over HTTP, so a deposit that lands in `pending` status can only
// be pushed to `succeeded` by something with backend access invoking the
// callback directly (a Go test helper, or a future ops/QA tool) - there is
// no browser-reachable way to complete it, by design (a real PSP webhook
// isn't reachable from the browser either).
export type DepositStatus = 'pending' | 'succeeded' | 'declined' | 'ambiguous' | 'reversed'

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
