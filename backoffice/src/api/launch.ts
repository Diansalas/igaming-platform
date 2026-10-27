// ADR 0112 launch authorisation client. Wire shapes are copied from
// docs/api/openapi/platform-api.yaml (LaunchRequest, LaunchReadiness, ...); no field is invented.
// Every function is a thin call: governance (four-eyes, proofs, readiness hashing, tenant scoping)
// is server-side and authoritative. A body tenant_id is never sent (the server ignores it anyway).
import { apiFetch, buildQuery } from './client'

export type SubjectStatus = 'pending_launch' | 'active' | 'suspended' | 'closed'
export type LaunchAction = 'activate' | 'reactivate' | 'suspend' | 'close' | 'ratify'
export type RequestableAction = Exclude<LaunchAction, 'suspend'>
export type LicensingModel = 'platform_licence' | 'tenant_licence' | 'other_manually_approved' | 'not_applicable_recorded_determination'
export type LicensingStatus = 'in_force' | 'applied_pending' | 'conditional' | 'suspended' | 'expired' | 'not_required_per_determination'
export type RequestStatus =
  | 'pending'
  | 'executing'
  | 'executed'
  | 'refused_at_execution'
  | 'rejected'
  | 'cancelled'
  | 'expired'
  | 'superseded'

export interface LaunchCheck {
  code: string
  result: 'pass' | 'fail' | 'not_evaluable'
  detail_code: string
}

export interface LaunchReadiness {
  version: number
  computed_at: string
  /** The fixed word `redacted` for a tenant-scoped caller. */
  environment: string
  checks: LaunchCheck[]
  readiness_hash: string
  non_passing_check_codes: string[]
  /** Live display data of R-PLAYER-FUNDS (not hashed). */
  counts?: Record<string, number>
  /** Rule ids only (R-RISK-RULES-LEGACY), capped per key; not hashed. */
  refs?: Record<string, string[]>
  /** DECIMAL STRINGS per asset. Never parse into a number. */
  balances?: Record<string, string>
  notices?: string[]
}

export interface LaunchApproval {
  id: string
  decision: 'approve' | 'reject'
  /** Omitted for a tenant-scoped caller. */
  decided_by?: string
  reason_code: string
  readiness_hash_at_decision: string
  decided_at: string
}

/**
 * ADR 0112 slice 6 (d): the closed catalogue of server reasons why THE CALLER cannot approve a request
 * now (internal/launchgov CannotApproveReasons). A code about the caller's own situation, never a name.
 */
export type CannotApproveReason =
  | 'not_platform_approver'
  | 'not_pending'
  | 'expired'
  | 'self_approval'
  | 'same_person'
  | 'no_person'
  | 'already_decided'
  | 'prior_reject_requires_change_note'

export interface LaunchPriorReject {
  request_id: string
  decided_at: string
  reason_code: string
  same_payload: boolean
}

export interface LaunchRequest {
  id: string
  tenant_id: string
  subject_kind: 'tenant' | 'brand'
  brand_id?: string
  action: LaunchAction
  from_status: SubjectStatus
  to_status: SubjectStatus
  status: RequestStatus
  licensing_model?: string
  licensing_status?: string
  licence_id?: string
  determination_reference?: string
  determination_by_role?: string
  responsible_operator_name?: string
  responsible_operator_registration?: string
  jurisdiction_ids: string[]
  conditions_note?: string
  readiness_snapshot_hash: string
  acknowledged_check_codes: string[]
  reason_code: string
  /** Omitted for a tenant-scoped caller when the requester is platform staff. */
  requested_by?: string
  requested_by_scope: 'platform' | 'tenant'
  required_approvals: number
  payload_hash: string
  created_at: string
  expires_at: string
  decided_at?: string
  refusal_code?: string
  prior_rejects?: LaunchPriorReject[]
  approvals?: LaunchApproval[]
  /** Platform-wide queue rows only (display text). */
  tenant_name?: string
  brand_name?: string
  /** Read responses (queue, list, detail): the server's advisory answer for the caller. Absent on write responses. */
  can_approve?: boolean
  cannot_approve_reason?: CannotApproveReason
}

export interface LaunchTransition {
  id: string
  subject_kind: 'tenant' | 'brand'
  brand_id?: string
  kind: 'governed' | 'legacy_baseline' | 'owner_provisioned'
  from_status?: string
  to_status: string
  request_id?: string
  /** Omitted for a tenant-scoped caller when platform staff executed it. */
  executed_by?: string
  approver_ids: string[]
  actor_type: 'system' | 'platform' | 'tenant'
  created_at: string
}

interface WithDisclaimer {
  disclaimer?: string
}

export interface LaunchReadinessResponse extends WithDisclaimer {
  readiness: LaunchReadiness
}
export interface LaunchRequestList extends WithDisclaimer {
  items: LaunchRequest[]
}
export interface LaunchQueueList extends WithDisclaimer {
  items: LaunchRequest[]
  /** Pass back as `cursor`; null on the last page. */
  next_cursor: string | null
}
export interface LaunchRequestEnvelope extends WithDisclaimer {
  request: LaunchRequest
}
export interface LaunchRequestDetail extends WithDisclaimer {
  request: LaunchRequest
  /** Present for a pending request: the CURRENT server-side readiness (its hash is the seen_readiness_hash). */
  readiness?: LaunchReadiness
}
export interface LaunchResult extends WithDisclaimer {
  request: LaunchRequest
  readiness: LaunchReadiness
  executed: boolean
  superseded_request_ids?: string[]
}
export interface LaunchDecisionResult extends WithDisclaimer {
  request: LaunchRequest
  decision: 'approve' | 'reject'
  executed: boolean
  readiness: LaunchReadiness
  prior_rejects: LaunchPriorReject[]
}
export interface LaunchTransitionList extends WithDisclaimer {
  items: LaunchTransition[]
}

export interface LaunchLicensing {
  model: LicensingModel
  status: LicensingStatus
  licence_id?: string
  determination_reference?: string
  determination_by_role?: string
}

export interface LaunchCreateBody {
  brand_id?: string
  action: RequestableAction
  reason_code: string
  licensing?: LaunchLicensing
  responsible_operator_name?: string
  responsible_operator_registration?: string
  jurisdiction_ids?: string[]
  conditions_note?: string
  acknowledged_check_codes?: string[]
}

export interface ReadinessParams {
  action: LaunchAction
  brand_id?: string
  /** Comma-separated jurisdiction ids. */
  jurisdiction_ids?: string
  licensing_model?: string
  licensing_status?: string
  licence_id?: string
}

const t = (id: string) => `/v1/admin/tenants/${encodeURIComponent(id)}`

export function getLaunchReadiness(tenantId: string, params: ReadinessParams): Promise<LaunchReadinessResponse> {
  return apiFetch(`${t(tenantId)}/launch-readiness${buildQuery(params)}`)
}

export function createLaunchRequest(tenantId: string, body: LaunchCreateBody): Promise<LaunchResult> {
  return apiFetch(`${t(tenantId)}/launch-authorisations`, { method: 'POST', body: JSON.stringify(body) })
}

export function listLaunchRequests(tenantId: string, params: { brand_id?: string; status?: string; limit?: number } = {}): Promise<LaunchRequestList> {
  return apiFetch(`${t(tenantId)}/launch-authorisations${buildQuery(params)}`)
}

export function getLaunchRequest(tenantId: string, requestId: string): Promise<LaunchRequestDetail> {
  return apiFetch(`${t(tenantId)}/launch-authorisations/${encodeURIComponent(requestId)}`)
}

export function approveLaunchRequest(tenantId: string, requestId: string, body: { reason_code: string; seen_readiness_hash: string }): Promise<LaunchDecisionResult> {
  return apiFetch(`${t(tenantId)}/launch-authorisations/${encodeURIComponent(requestId)}/approve`, { method: 'POST', body: JSON.stringify(body) })
}

/** seen_readiness_hash is not required to reject. */
export function rejectLaunchRequest(tenantId: string, requestId: string, body: { reason_code: string }): Promise<LaunchDecisionResult> {
  return apiFetch(`${t(tenantId)}/launch-authorisations/${encodeURIComponent(requestId)}/reject`, { method: 'POST', body: JSON.stringify(body) })
}

export function cancelLaunchRequest(tenantId: string, requestId: string, body: { reason_code: string }): Promise<LaunchRequestEnvelope> {
  return apiFetch(`${t(tenantId)}/launch-authorisations/${encodeURIComponent(requestId)}/cancel`, { method: 'POST', body: JSON.stringify(body) })
}

export interface SuspendBody {
  reason_code: string
  conditions_note?: string
}

/** brandId absent suspends the tenant itself (platform staff only; the server enforces it). */
export function suspendSubject(tenantId: string, brandId: string | undefined, body: SuspendBody): Promise<LaunchResult> {
  const path = brandId ? `${t(tenantId)}/brands/${encodeURIComponent(brandId)}/suspend` : `${t(tenantId)}/suspend`
  return apiFetch(path, { method: 'POST', body: JSON.stringify(body) })
}

export function listLaunchHistory(tenantId: string, params: { brand_id?: string; limit?: number } = {}): Promise<LaunchTransitionList> {
  return apiFetch(`${t(tenantId)}/launch-status-history${buildQuery(params)}`)
}

/** Platform-wide approval queue (platform staff only; a tenant caller is 403). */
export function listLaunchQueue(params: { status?: string; limit?: number; cursor?: string } = {}): Promise<LaunchQueueList> {
  return apiFetch(`/v1/admin/launch-authorisations${buildQuery(params)}`)
}

// Registry reads exist for platform_admin only (jurisdiction_registry:manage); a tenant caller gets 403 and
// types the ids instead (API gap reported).
export interface Jurisdiction {
  id: string
  code: string
  name: string
}
export interface Licence {
  id: string
  jurisdiction_id: string
  licensee: 'platform' | 'tenant'
  licence_number: string
  status: string
}
export function listJurisdictions(): Promise<Jurisdiction[]> {
  return apiFetch('/v1/admin/jurisdictions')
}
export function listLicences(): Promise<Licence[]> {
  return apiFetch('/v1/admin/licences')
}
