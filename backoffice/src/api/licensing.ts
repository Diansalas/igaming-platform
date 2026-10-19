// Licensing and jurisdiction client (lic5). Wire shapes are copied from docs/api/openapi/platform-api.yaml (Jurisdiction,
// LicenceDetail, LicenceVersion, LicenceCountryCeiling, OperatingCountryPolicy, TenantJurisdictionConfig,
// LicensingChange, LicensingChangeResponse, ...); no field is invented. Facts the UI relies on:
//  - every route here is platform_admin only (jurisdiction_registry:read / :manage, licensing_change:*, tenant_licence:assign);
//    the server is authoritative and every refusal is rendered;
//  - the filing routes (POST /v1/admin/licences, PUT /v1/admin/tenants/{id}/licence, PUT /v1/admin/licences/{id}/status |
//    validity-dates | review-due | evidence-reference) answer 202 {change_request, executed:false} when the change WIDENS and
//    needs two approvals, and 200 when a narrowing / neutral change executed at once. apiFetch returns the body only, so the
//    two answers are told apart by SHAPE (`change_request` vs `licence`) and a body that matches neither is "unconfirmed",
//    never success;
//  - filing and approving are step-up `critical` (the apiFetch interceptor prompts and re-sends once); reject and cancel
//    need none. The signed actor proof (ADR 0110) is produced server-side: nothing here signs or hashes;
//  - POST .../cancel takes NO request body (OpenAPI), so no reason code is sent for it;
//  - a licence evidence reference is an opaque string supplied by the operator and NOT verified by the platform.
import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export interface Jurisdiction {
  id: string
  code: string
  name: string
  regulatory_body?: string
  notes?: string
  created_at: string
}

export type LicenceStatus = 'active' | 'suspended' | 'expired'
export type Licensee = 'platform' | 'tenant'

export interface Licence {
  id: string
  jurisdiction_id: string
  licensee: Licensee
  licence_number: string
  status: LicenceStatus
  permitted_products: string[]
  permitted_markets: string[]
  created_at: string
  issued_at: string | null
  expires_at: string | null
  review_due_at: string | null
  licence_evidence_reference: string | null
  updated_at: string
}

export interface LicenceDetail extends Licence {
  tenant_ids: string[]
}

export interface LicenceVersion {
  version_no: number
  operation: 'baseline' | 'insert' | 'update'
  changed_at: string
  changed_by: string | null
  status: string
  issued_at: string | null
  expires_at: string | null
  review_due_at: string | null
  licence_evidence_reference: string | null
  licence_number: string
}

export interface LicenceCountryCeiling {
  id: string
  licence_id: string
  country_code: string
  state: 'enabled' | 'disabled'
  status: 'active' | 'withdrawn'
  authorization_reference: string | null
  reason_code: string
  policy_version: string
  effective_from: string
  effective_to: string | null
  created_by_actor_type: string
  created_by: string | null
  created_at: string
}

export interface OperatingCountryPolicy {
  id: string
  tenant_id: string
  scope: 'tenant' | 'brand' | 'operation'
  brand_id: string | null
  operation_code: string | null
  product_code: string | null
  country_code: string
  state: 'enabled' | 'disabled'
  status: 'active' | 'withdrawn'
  authorization_reference: string | null
  reason_code: string
  policy_version: string
  effective_from: string
  effective_to: string | null
  created_by_actor_type: string
  created_by: string | null
  created_at: string
}

export interface TenantJurisdictionConfig {
  id: string
  tenant_id: string
  jurisdiction_id: string
  kyc_ruleset_id: string | null
  aml_ruleset_id: string | null
  rg_ruleset_id: string | null
  reporting_ruleset_id: string | null
  allowed_currencies: string[]
  allowed_payment_methods: string[]
  geo_block_list: string[]
  effective_from: string
  effective_to: string | null
}

export type ChangeStatus = 'requested' | 'approved' | 'executed' | 'rejected' | 'cancelled' | 'expired'

export interface LicensingChangeApproval {
  approver_id: string | null
  reason_code: string
  decided_at: string
}

export interface LicensingChange {
  id: string
  kind: string
  payload: Record<string, unknown>
  payload_hash: string
  reason_code: string
  note?: string | null
  widening: boolean
  required_approvals: number
  subject_tenant_id?: string | null
  subject_licence_id?: string | null
  requested_by?: string | null
  requested_scope: 'platform' | 'tenant'
  status: ChangeStatus
  created_at: string
  expires_at: string
  decided_by?: string | null
  decided_at?: string | null
  decision_reason_code?: string | null
  executed_by?: string | null
  executed_at?: string | null
  approvals: LicensingChangeApproval[]
}

export interface LicensingChangeResponse {
  change_request: LicensingChange
  executed: boolean
  outcome?: Record<string, unknown>
  disclaimer: string
}

export interface LicensingChangeList {
  items: LicensingChange[]
  disclaimer: string
}

export interface LicenceChange {
  licence: Licence
  change_kind: 'widening' | 'narrowing' | 'neutral'
  change_request_id?: string
}

export interface TenantLicenceAssignment {
  tenant_id: string
  licence_id: string | null
  change_request_id?: string
}

/** Result of a filing call: either filed and pending (202), applied at once (200), or an answer we cannot classify. */
export type FilingResult =
  | { kind: 'pending'; response: LicensingChangeResponse }
  | { kind: 'applied'; change: LicenceChange }
  | { kind: 'unbound'; assignment: TenantLicenceAssignment }
  | { kind: 'unconfirmed' }

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

/** Tells the 202 / 200 answers of the filing routes apart by shape. Anything else is `unconfirmed`: never success. */
export function classifyFiling(body: unknown): FilingResult {
  if (isObject(body) && isObject(body.change_request) && typeof body.change_request.id === 'string') {
    return { kind: 'pending', response: body as unknown as LicensingChangeResponse }
  }
  if (isObject(body) && isObject(body.licence) && typeof body.change_kind === 'string') {
    return { kind: 'applied', change: body as unknown as LicenceChange }
  }
  if (isObject(body) && typeof body.tenant_id === 'string' && body.licence_id === null) {
    return { kind: 'unbound', assignment: body as unknown as TenantLicenceAssignment }
  }
  return { kind: 'unconfirmed' }
}

/** reason_code format from the OpenAPI schema: 1..64 characters of [A-Za-z0-9_.:-], first one alphanumeric. */
export const REASON_CODE_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$/
export const isReasonCode = (s: string) => REASON_CODE_PATTERN.test(s)

// --- reads (jurisdiction_registry:read, licensing_change:read: platform_admin only) ---

export function listJurisdictions(): Promise<Jurisdiction[]> {
  return apiFetch('/v1/admin/jurisdictions')
}

export function getJurisdiction(jurisdictionId: string): Promise<Jurisdiction> {
  return apiFetch(`/v1/admin/jurisdictions/${encodeURIComponent(jurisdictionId)}`)
}

/** Not paginated: a bare array. */
export function listLicences(): Promise<LicenceDetail[]> {
  return apiFetch('/v1/admin/licences')
}

export function getLicence(licenceId: string): Promise<LicenceDetail> {
  return apiFetch(`/v1/admin/licences/${encodeURIComponent(licenceId)}`)
}

export function listLicenceVersions(licenceId: string): Promise<LicenceVersion[]> {
  return apiFetch(`/v1/admin/licences/${encodeURIComponent(licenceId)}/versions`)
}

export interface CeilingParams {
  licence_id: string
  country_code?: string
  current?: boolean
  limit?: number
  offset?: number
}

export function listLicenceCountryCeilings(params: CeilingParams): Promise<PagedResponse<LicenceCountryCeiling>> {
  return apiFetch(`/v1/admin/licence-country-ceilings${buildQuery(params)}`)
}

export interface OperatingPolicyParams {
  tenant_id: string
  country_code?: string
  current?: boolean
  limit?: number
  offset?: number
}

export function listOperatingCountryPolicies(params: OperatingPolicyParams): Promise<PagedResponse<OperatingCountryPolicy>> {
  return apiFetch(`/v1/admin/operating-country-policies${buildQuery(params)}`)
}

export interface PageParams {
  limit?: number
  offset?: number
}

export function listTenantJurisdictionConfigs(tenantId: string, params: PageParams = {}): Promise<PagedResponse<TenantJurisdictionConfig>> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/jurisdiction-configs${buildQuery(params)}`)
}

export interface ChangeListParams {
  status?: ChangeStatus
  kind?: string
  limit?: number
}

export function listLicensingChanges(params: ChangeListParams = {}): Promise<LicensingChangeList> {
  return apiFetch(`/v1/admin/licensing-changes${buildQuery(params)}`)
}

export function getLicensingChange(changeId: string): Promise<LicensingChangeResponse> {
  return apiFetch(`/v1/admin/licensing-changes/${encodeURIComponent(changeId)}`)
}

// --- filing (existing change-filing endpoints; step-up critical; 202 when widening) ---

export interface CreateLicenceBody {
  jurisdiction_id: string
  licensee: Licensee
  licence_number: string
  permitted_products: string[]
  permitted_markets: string[]
  reason_code: string
}

export function createLicence(body: CreateLicenceBody): Promise<unknown> {
  return apiFetch('/v1/admin/licences', { method: 'POST', body: JSON.stringify(body) })
}

/** `licenceId` is a UUID (bind or rebind: widening, 202) or null (unbind: narrowing, executes, 200). The key is always sent. */
export function setTenantLicence(tenantId: string, licenceId: string | null, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/licence`, {
    method: 'PUT',
    body: JSON.stringify({ licence_id: licenceId, reason_code: reasonCode }),
  })
}

export function setLicenceStatus(licenceId: string, status: LicenceStatus, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/licences/${encodeURIComponent(licenceId)}/status`, {
    method: 'PUT',
    body: JSON.stringify({ status, reason_code: reasonCode }),
  })
}

/** Both dates are required keys; null clears. */
export function setLicenceValidityDates(licenceId: string, issuedAt: string | null, expiresAt: string | null, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/licences/${encodeURIComponent(licenceId)}/validity-dates`, {
    method: 'PUT',
    body: JSON.stringify({ issued_at: issuedAt, expires_at: expiresAt, reason_code: reasonCode }),
  })
}

export function setLicenceReviewDue(licenceId: string, reviewDueAt: string | null, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/licences/${encodeURIComponent(licenceId)}/review-due`, {
    method: 'PUT',
    body: JSON.stringify({ review_due_at: reviewDueAt, reason_code: reasonCode }),
  })
}

/** An opaque reference supplied by the operator (1..200 chars); null clears. Never verified by the platform. */
export function setLicenceEvidenceReference(licenceId: string, reference: string | null, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/licences/${encodeURIComponent(licenceId)}/evidence-reference`, {
    method: 'PUT',
    body: JSON.stringify({ licence_evidence_reference: reference, reason_code: reasonCode }),
  })
}

// --- decisions ---

/** Approve: step-up critical. The final approval executes the change in the same transaction (executed: true). */
export function approveLicensingChange(changeId: string, reasonCode: string): Promise<LicensingChangeResponse> {
  return apiFetch(`/v1/admin/licensing-changes/${encodeURIComponent(changeId)}/approve`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }) })
}

/** Reject: no step-up. */
export function rejectLicensingChange(changeId: string, reasonCode: string): Promise<LicensingChangeResponse> {
  return apiFetch(`/v1/admin/licensing-changes/${encodeURIComponent(changeId)}/reject`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }) })
}

/** Cancel (the requester only): no step-up and no request body. */
export function cancelLicensingChange(changeId: string): Promise<LicensingChangeResponse> {
  return apiFetch(`/v1/admin/licensing-changes/${encodeURIComponent(changeId)}/cancel`, { method: 'POST' })
}
