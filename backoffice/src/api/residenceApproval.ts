// Residence determination maker-checker client (lic10, migration 0181). Wire shapes are copied from
// docs/api/openapi/platform-api.yaml (ResidenceApprovalPolicy, ResidenceApprovalPolicyRequest, ResidenceApprovalPolicyChange,
// ResidenceApprovalPolicyApproval, ResidenceReasonRequest, PendingResidenceApproval, ResidenceDeterminationApprovalRequest).
// Facts the UI relies on:
//  - these routes are TENANT-SCOPED staff only (compliance of the tenant; tenant_admin reads). A platform token is refused 403 and
//    there is no platform_acting path. The tenant id is therefore the signed-in staff member's own tenant;
//  - setting a mode answers 200 {status:'active'} for a STRICTER mode (active at once) and 202 {status:'pending'} for a LESS strict
//    one (two approvals by two distinct Persons). apiFetch returns the body only, so the two are told apart by the BODY shape and
//    anything else is "unconfirmed", never success. Setting a mode needs no step-up; approving does (critical);
//  - a pending determination is NOT market evidence. The list carries ids and times only, never the country;
//  - `determined_at` is passed back exactly as listed (microsecond precision string): it is never parsed or re-formatted.
import { apiFetch } from './client'

export type ResidenceMode = 'none' | 'into_open_market' | 'all'
export const RESIDENCE_MODES: readonly ResidenceMode[] = ['none', 'into_open_market', 'all']

export interface ResidencePolicyVersion {
  id: string
  mode: ResidenceMode
  previous_mode: ResidenceMode
  status: 'pending' | 'active'
  required_approvals: number
  approvals: number
  reason_code: string
  requested_by?: string | null
  requested_at: string
  activated_at?: string | null
}

export interface ResidencePolicy {
  effective_mode: ResidenceMode
  versions: ResidencePolicyVersion[]
}

export interface ResidencePolicyChange {
  policy_id: string
  status: 'active' | 'pending'
  required_approvals: number
  previous_mode: ResidenceMode
  requested_mode: ResidenceMode
}

export interface ResidencePolicyApproval {
  status: 'active' | 'pending'
  approvals: number
  required_approvals: number
}

export interface PendingResidenceApproval {
  verification_id: string
  player_account_id: string
  brand_id: string
  determined_at: string
  determined_by?: string | null
  policy_mode: 'into_open_market' | 'all'
}

export function getResidencePolicy(tenantId: string): Promise<ResidencePolicy> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/residence-approval-policy`)
}

/** Step-up none. 200 = stricter, active at once; 202 = less strict, pending two approvals. */
export function setResidencePolicy(tenantId: string, mode: ResidenceMode, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/residence-approval-policy`, {
    method: 'POST',
    body: JSON.stringify({ mode, reason_code: reasonCode }),
  })
}

/** Step-up critical. The approval that reaches the required count activates the policy in the same transaction. */
export function approveResidencePolicy(tenantId: string, policyId: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/residence-approval-policy/${encodeURIComponent(policyId)}/approvals`, {
    method: 'POST',
    body: JSON.stringify({ reason_code: reasonCode }),
  })
}

/** Bare array, oldest first, at most 200. */
export function listPendingResidenceApprovals(): Promise<PendingResidenceApproval[]> {
  return apiFetch('/v1/admin/kyc/residence-determinations/pending')
}

/** Step-up critical. `determinedAt` is the exact string listed for the determination. */
export function approveResidenceDetermination(verificationId: string, determinedAt: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/kyc/verifications/${encodeURIComponent(verificationId)}/residence-approval`, {
    method: 'POST',
    body: JSON.stringify({ determined_at: determinedAt, reason_code: reasonCode }),
  })
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}
const isMode = (v: unknown): v is ResidenceMode => v === 'none' || v === 'into_open_market' || v === 'all'

export type ResidenceSetResult =
  | { kind: 'applied'; change: ResidencePolicyChange }
  | { kind: 'pending'; change: ResidencePolicyChange }
  | { kind: 'unconfirmed' }

/**
 * `applied` only for status 'active' with 0 required approvals; `pending` only for status 'pending' with 2. The body must also
 * echo the requested mode. Everything else is `unconfirmed`.
 */
export function classifyResidenceSet(body: unknown, requestedMode: ResidenceMode): ResidenceSetResult {
  if (!isObject(body) || typeof body.policy_id !== 'string' || !isMode(body.previous_mode) || body.requested_mode !== requestedMode || typeof body.required_approvals !== 'number') {
    return { kind: 'unconfirmed' }
  }
  const change = body as unknown as ResidencePolicyChange
  if (body.status === 'active' && body.required_approvals === 0) return { kind: 'applied', change }
  if (body.status === 'pending' && body.required_approvals === 2) return { kind: 'pending', change }
  return { kind: 'unconfirmed' }
}

export type ResidenceApprovalResult =
  | { kind: 'activated'; approval: ResidencePolicyApproval }
  | { kind: 'recorded'; approval: ResidencePolicyApproval }
  | { kind: 'unconfirmed' }

export function classifyResidencePolicyApproval(body: unknown): ResidenceApprovalResult {
  if (!isObject(body) || typeof body.approvals !== 'number' || typeof body.required_approvals !== 'number') return { kind: 'unconfirmed' }
  const approval = body as unknown as ResidencePolicyApproval
  if (body.status === 'active' && body.approvals >= body.required_approvals) return { kind: 'activated', approval }
  if (body.status === 'pending' && body.approvals < body.required_approvals) return { kind: 'recorded', approval }
  return { kind: 'unconfirmed' }
}

/** Confirmed only when the body names the same verification and says approved. */
export function isDeterminationApproved(body: unknown, verificationId: string): boolean {
  return isObject(body) && body.status === 'approved' && body.verification_id === verificationId
}

/** Strictness order used for the on-screen HINT only; the database compares against the mode in force. */
export function modeRank(m: ResidenceMode): number {
  return RESIDENCE_MODES.indexOf(m)
}
