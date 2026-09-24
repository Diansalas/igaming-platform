import { apiFetch } from './client'

// Read-only player views of KYC and responsible-gaming state, plus the
// existing self-service "start identity verification" action. Shapes
// mirror internal/httpserver/kyc_handlers.go's verificationResponse and
// rg_handlers.go's restrictionResponse (both endpoints return bare arrays).

export interface KycVerification {
  id: string
  status: string
  provider_id: string
  reason?: string
  reviewed_at?: string
  created_at: string
}

export function listMyKycVerifications(): Promise<KycVerification[]> {
  return apiFetch<KycVerification[]>('/v1/me/kyc/verifications')
}

/** POST /v1/me/kyc/verifications - opens a verification case with the tenant's KYC provider (no body). */
export function startKycVerification(): Promise<KycVerification> {
  return apiFetch<KycVerification>('/v1/me/kyc/verifications', { method: 'POST' })
}

export interface RgRestriction {
  id: string
  restriction_type: string
  scope: string
  starts_at: string
  ends_at?: string
  indefinite: boolean
  source: string
  active: boolean
}

export function getMyRgStatus(): Promise<RgRestriction[]> {
  return apiFetch<RgRestriction[]>('/v1/me/rg/status')
}
