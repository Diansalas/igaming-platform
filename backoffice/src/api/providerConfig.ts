import { apiFetch } from './client'

/** amountLimitRequest (internal/httpserver/provider_capability_handlers.go). Amounts are integer minor units. */
export interface AmountLimit {
  asset_code: string
  min_amount: number
  max_amount: number
}

/**
 * writeCapabilityRequest - PUT /v1/admin/providers/{providerID}/capability
 * (PermProviderConfigWrite + RequireTenantScope: tenant_admin only). The
 * backend can only NARROW what the adapter declares; a widening attempt
 * is a 400 whose message the UI shows verbatim.
 */
export interface PaymentCapabilityRequest {
  supported_fiat_currencies: string[]
  supported_crypto_assets: string[]
  supported_payment_methods: string[]
  supported_countries: string[]
  supports_deposit: boolean
  supports_withdrawal: boolean
  supports_refund_reversal: boolean
  amount_limits: AmountLimit[]
  priority: number
  status: 'active' | 'disabled'
}

/**
 * writeCapabilityResponse (SA-5): the row id plus the configuration that was actually STORED (read back after the
 * write) and `narrowed`, true if that differs from the request. A request outside the adapter's manifest is a 400, so
 * `narrowed` is false on every 200 today. `effective` and `narrowed` are optional here only so an older server that
 * returns just `{ id }` still works.
 */
export interface PaymentCapabilityResponse {
  id: string
  effective?: PaymentCapabilityRequest
  narrowed?: boolean
}

/** There is no GET for this resource; the PUT response is the only read-back. */
export function writePaymentCapability(providerId: string, body: PaymentCapabilityRequest): Promise<PaymentCapabilityResponse> {
  return apiFetch(`/v1/admin/providers/${encodeURIComponent(providerId)}/capability`, {
    method: 'PUT',
    body: JSON.stringify(body),
  })
}
