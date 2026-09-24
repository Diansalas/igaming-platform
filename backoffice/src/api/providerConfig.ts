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

/** The handler returns only the capability row id. There is no GET for this resource. */
export function writePaymentCapability(providerId: string, body: PaymentCapabilityRequest): Promise<{ id: string }> {
  return apiFetch(`/v1/admin/providers/${encodeURIComponent(providerId)}/capability`, {
    method: 'PUT',
    body: JSON.stringify(body),
  })
}
