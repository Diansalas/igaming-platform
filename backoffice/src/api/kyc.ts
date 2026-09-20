import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export type KycStatus = 'unverified' | 'pending' | 'review_required' | 'approved' | 'rejected' | 'expired'

export interface KycCase {
  id: string
  player_account_id: string
  brand_id: string
  status: KycStatus
  provider_id: string
  provider_reference?: string
  reason?: string
  submitted_at?: string
  reviewed_at?: string
  reviewed_by?: string
  expires_at?: string
  created_at: string
  updated_at: string
  has_verified_residence: boolean
}

export interface ListKycCasesParams {
  status?: KycStatus
  player_account_id?: string
  limit?: number
  offset?: number
}

export function listKycCases(params: ListKycCasesParams): Promise<PagedResponse<KycCase>> {
  return apiFetch(`/v1/admin/kyc/cases${buildQuery(params)}`)
}

export interface ReviewVerificationParams {
  status: 'approved' | 'rejected' | 'review_required'
  reason: string
  verifiedResidenceCountry?: string | null
}

/**
 * The only existing KYC reviewer-decision mutation (already-existing
 * endpoint, per the Stage 5 directive's explicit "do not invent new KYC
 * decision endpoints" instruction) - approves/rejects/re-queues a
 * verification. Gated server-side by PermVerificationReview
 * (RoleCompliance only); a tenant_admin viewing the case queue will see
 * this action rejected with a 403 from the API, not hidden by this UI.
 */
export function reviewVerification(id: string, params: ReviewVerificationParams): Promise<KycCase> {
  return apiFetch(`/v1/admin/kyc/verifications/${id}/review`, {
    method: 'POST',
    body: JSON.stringify({
      status: params.status,
      reason: params.reason,
      verified_residence_country: params.verifiedResidenceCountry ?? null,
    }),
  })
}
