// KYC enforcement policy client (lic10, L-20). Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (KYCEnforcementPolicy, KYCEnforcementPolicyCreateRequest, KYCEnforcementPolicyReasonRequest, KYCEnforcementPolicySupersedeRequest).
// Facts the UI relies on:
//  - every route is platform_admin only (kyc_enforcement_policy:read | :manage); a tenant token is refused 403;
//  - every write is step-up `critical` (the apiFetch interceptor prompts and re-sends once);
//  - the platform ships NO policy and NO threshold: every value is supplied by a human decision of compliance, with a legal
//    review reference. Nothing here has a default threshold, asset, jurisdiction or operation;
//  - four-eyes is enforced by the DATABASE: the activating administrator must differ from the author; a supersede must differ
//    from the authors of BOTH rows. A refusal is a 403 and is shown, never worked around;
//  - an ACTIVE policy is never switched off: it can only be superseded by an activated successor (there is no deactivate call);
//  - `threshold_minor_units` is a decimal STRING of integer minor units (1..38 digits). It is never converted to a number.
import { apiFetch, buildQuery } from './client'

export type KycTriggerType = 'cumulative_deposit' | 'edd_amount' | 'registration_tier' | 'play'
/** The only trigger types the API accepts for authoring (the evaluator consults nothing else). */
export type AuthorableTriggerType = 'cumulative_deposit' | 'play'
export type KycPolicyStatus = 'draft' | 'active' | 'withdrawn'
export type PlayOperation = 'casino_play' | 'sportsbook_play'

export interface KycEnforcementPolicy {
  id: string
  licensing_jurisdiction_id: string
  trigger_type: KycTriggerType
  status: KycPolicyStatus
  threshold_minor_units?: string | null
  asset_code?: string | null
  required_tier?: string | null
  play_operation?: PlayOperation | null
  effective_from: string
  legal_review_reference?: string | null
  reason_code: string
  created_at: string
  created_by?: string | null
  successor_policy_id?: string | null
}

export interface KycPolicyListParams {
  licensing_jurisdiction_id?: string
  status?: KycPolicyStatus
}

/** Bare array, newest first, at most 500. */
export function listKycEnforcementPolicies(params: KycPolicyListParams = {}): Promise<KycEnforcementPolicy[]> {
  return apiFetch(`/v1/admin/kyc/enforcement-policies${buildQuery(params)}`)
}

export interface CreateKycPolicyBody {
  licensing_jurisdiction_id: string
  trigger_type: AuthorableTriggerType
  /** cumulative_deposit only (omitted for play: the server refuses it there). */
  threshold_minor_units?: string
  asset_code?: string
  /** play only. */
  play_operation?: PlayOperation
  legal_review_reference: string
  reason_code: string
}

export function createKycEnforcementPolicy(body: CreateKycPolicyBody): Promise<unknown> {
  return apiFetch('/v1/admin/kyc/enforcement-policies', { method: 'POST', body: JSON.stringify(body) })
}

export function activateKycEnforcementPolicy(policyId: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/kyc/enforcement-policies/${encodeURIComponent(policyId)}/activate`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }) })
}

/** A never-activated draft only. There is no call that switches an active policy off. */
export function withdrawKycEnforcementPolicy(policyId: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/kyc/enforcement-policies/${encodeURIComponent(policyId)}/withdraw`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }) })
}

/** `policyId` is the ACTIVE policy being replaced; the successor is a DRAFT of the same enforcement key. */
export function supersedeKycEnforcementPolicy(policyId: string, successorPolicyId: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/kyc/enforcement-policies/${encodeURIComponent(policyId)}/supersede`, {
    method: 'POST',
    body: JSON.stringify({ successor_policy_id: successorPolicyId, reason_code: reasonCode }),
  })
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

export type PolicyOutcome = { kind: 'confirmed'; policy: KycEnforcementPolicy } | { kind: 'unconfirmed' }

/** A write is confirmed only when the body is a policy with the expected status (and, when known, the expected id). */
export function classifyKycPolicyWrite(body: unknown, expectStatus: KycPolicyStatus, expectId?: string): PolicyOutcome {
  if (!isObject(body) || typeof body.id !== 'string' || body.status !== expectStatus) return { kind: 'unconfirmed' }
  if (expectId !== undefined && body.id !== expectId) return { kind: 'unconfirmed' }
  return { kind: 'confirmed', policy: body as unknown as KycEnforcementPolicy }
}

/** 1..38 digits, no leading zero: a positive integer string (mirrors the server's thresholdRE). */
export const THRESHOLD_PATTERN = /^[1-9][0-9]{0,37}$/
export const ASSET_CODE_PATTERN = /^[A-Za-z0-9_.-]{1,32}$/
