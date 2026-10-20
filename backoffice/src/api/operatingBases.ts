// Operating bases client (lic12, over lic9 / migration 0180). Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (OperatingBasis, OperatingBasisDecision, OperatingBasisDetail, OperatingBasisPage, LicensingChangeCreateRequest). Facts relied on:
//  - reads are platform_admin only (jurisdiction_registry:read); `tenant_id` is the platform operator's EXPLICIT target and is a
//    required query parameter. A tenant has no read API of its own;
//  - bases are written ONLY by filing a licensing change through POST /v1/admin/licensing-changes (kinds operating_basis_set,
//    operating_basis_withdraw, operating_basis_review_record). That route answers 201 with {change_request, executed}:
//    executed:false means PENDING APPROVAL (a widening change needs two other people), executed:true means a single-actor narrowing
//    change was applied in the same call. apiFetch returns the body only, so `classifyBasisFiling` reads the BODY, never the status
//    line, and anything it cannot recognise is "unconfirmed", never success;
//  - filing is step-up `critical` (the apiFetch interceptor prompts and re-sends the same body once). The signed actor proof
//    (ADR 0110) is produced server-side: nothing here signs, hashes or sends a proof;
//  - a basis is a RECORDED BUSINESS DECISION. It is not a legal or regulatory authorisation, and an evidence / determination
//    reference is an opaque string supplied by the operator that the platform does not verify.
import { apiFetch, buildQuery } from './client'
import type { LicensingChangeResponse } from './licensing'
import type { PagedResponse } from './types'

export type BasisKind = 'platform_licence' | 'partner_licence' | 'no_licence_documented'
export type BasisStatus = 'active' | 'withdrawn'

export interface OperatingBasis {
  id: string
  tenant_id: string
  brand_id: string | null
  country_code: string
  operation_code: string
  product_code: string | null
  basis_kind: BasisKind
  licence_id: string | null
  jurisdiction_id: string | null
  determination_reference: string
  determined_by_role: string
  basis_evidence_reference: string | null
  restrictions_note: string | null
  restricted_product_codes: string[]
  review_due_at: string | null
  needs_rereview: boolean
  last_review_reference: string | null
  status: BasisStatus
  reason_code: string
  policy_version: string
  effective_from: string
  effective_to: string | null
  change_request_id: string | null
  created_by: string | null
  created_at: string
  disclaimer: string
}

export interface OperatingBasisDecision {
  change_request_id: string
  kind: string
  reason_code: string
  requested_by: string | null
  executed_by: string | null
  executed_at: string | null
  approvals: { approver_id: string | null }[]
}

export interface OperatingBasisDetail extends OperatingBasis {
  decision: OperatingBasisDecision | null
}

export interface OperatingBasisParams {
  tenant_id: string
  country_code?: string
  current?: boolean
  limit?: number
  offset?: number
}

export function listOperatingBases(params: OperatingBasisParams): Promise<PagedResponse<OperatingBasis>> {
  return apiFetch(`/v1/admin/operating-bases${buildQuery(params)}`)
}

export function getOperatingBasis(basisId: string): Promise<OperatingBasisDetail> {
  return apiFetch(`/v1/admin/operating-bases/${encodeURIComponent(basisId)}`)
}

// --- filing (the generic licensing-change route; step-up critical) ---

export type BasisChangeKind = 'operating_basis_set' | 'operating_basis_withdraw' | 'operating_basis_review_record'

/** The scope keys every basis payload carries. `null` means tenant-wide (brand) / all products (product). Every key is always sent. */
export interface BasisScopePayload {
  tenant_id: string
  brand_id: string | null
  country_code: string
  operation_code: string
  product_code: string | null
}

export interface BasisSetPayload extends BasisScopePayload {
  basis_kind: BasisKind
  licence_id: string | null
  jurisdiction_id: string | null
  determination_reference: string
  determined_by_role: string
  basis_evidence_reference: string | null
  restrictions_note: string | null
  restricted_product_codes: string[]
  review_due_at: string | null
}

export type BasisWithdrawPayload = BasisScopePayload

export interface BasisReviewPayload extends BasisScopePayload {
  review_due_at: string
  needs_rereview: boolean
  review_reference: string
}

export interface FileBasisChangeBody {
  kind: BasisChangeKind
  payload: BasisSetPayload | BasisWithdrawPayload | BasisReviewPayload
  reason_code: string
}

export function fileBasisChange(body: FileBasisChangeBody): Promise<unknown> {
  return apiFetch('/v1/admin/licensing-changes', {
    method: 'POST',
    body: JSON.stringify({ kind: body.kind, payload: body.payload, reason_code: body.reason_code }),
  })
}

/** Result of filing: pending approval, applied at once, or an answer we cannot classify (never success). */
export type BasisFilingResult =
  | { kind: 'pending'; response: LicensingChangeResponse }
  | { kind: 'applied'; response: LicensingChangeResponse }
  | { kind: 'unconfirmed' }

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

/**
 * `applied` only when the server says executed:true AND the request itself reads `executed`. `pending` only when
 * executed:false AND the request is still open (`requested`). Everything else is `unconfirmed`.
 */
export function classifyBasisFiling(body: unknown): BasisFilingResult {
  if (!isObject(body) || !isObject(body.change_request) || typeof body.change_request.id !== 'string' || typeof body.executed !== 'boolean') {
    return { kind: 'unconfirmed' }
  }
  const status = body.change_request.status
  if (body.executed === true && status === 'executed') return { kind: 'applied', response: body as unknown as LicensingChangeResponse }
  if (body.executed === false && status === 'requested') return { kind: 'pending', response: body as unknown as LicensingChangeResponse }
  return { kind: 'unconfirmed' }
}
