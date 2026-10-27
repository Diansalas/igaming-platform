// Withdrawal restriction policy client (lic11, migration 0182). Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (WithdrawalRestrictionPolicyEffective / Cause / Stage / Version(s) / Change / ChangeResponse / ChangeListResponse).
// Facts the UI relies on:
//  - reads: platform_admin any tenant; tenant_admin and compliance their OWN tenant only. The change list and a change are
//    platform-only reads. Filing, approving, rejecting and cancelling are platform_admin only (withdrawal_restriction_policy:change);
//  - filing answers 201 for BOTH outcomes ({change, executed}); executed:true = a tightening (or equal) change applied at once,
//    executed:false = a loosening change pending two approvals by two distinct Persons. The class is computed by the database, so
//    outcomes are read from the BODY and anything else is "unconfirmed";
//  - filing and approving are step-up `critical`; reject and cancel need none; cancel takes no body;
//  - only the four licence-state causes are configurable. Tenant / brand status and market closure are NOT configurable here
//    (owner decisions, ADR 0095 section 44) and are shown as information only;
//  - hard restrictions (AML / sanctions holds, KYC, risk, kill switch, account status) are not affected by any policy row.
import { apiFetch, buildQuery } from './client'

export type WrpAction = 'allow' | 'hold_for_review' | 'block'
export const WRP_ACTIONS: readonly WrpAction[] = ['allow', 'hold_for_review', 'block']
export type WrpStage = 'request' | 'dispatch' | 'redispatch'
export type WrpConfigurableCause = 'licence_suspended' | 'licence_expired' | 'licence_not_yet_issued' | 'licence_missing'
export type WrpChangeStatus = 'requested' | 'approved' | 'executed' | 'rejected' | 'cancelled' | 'expired'

export interface WrpStageRow {
  stage: WrpStage
  action: WrpAction
  source: 'configured' | 'default_unconfigured'
  policy_id: string | null
  set_version: number | null
}

export interface WrpCause {
  cause: string
  class: 'licence' | 'subject_status' | 'market'
  configurable: boolean
  evaluated: boolean
  governing_reference: string
  current_behaviour: string
  stages: WrpStageRow[]
  unconfigured: boolean
  notice?: string
}

export interface WrpEffective {
  tenant_id: string
  set_version: number
  configured: boolean
  causes: WrpCause[]
  disclaimer: string
}

export interface WrpVersion {
  id: string
  cause: string
  action: WrpAction
  set_version: number
  change_id: string | null
  reason_code: string
  created_by: string | null
  created_at: string
}

export interface WrpApproval {
  approver_id: string | null
  reason_code: string
  decided_at: string
}

export interface WrpChange {
  id: string
  tenant_id: string
  cause: string
  action: WrpAction
  reason_code: string
  note?: string | null
  payload_hash: string
  widening: boolean
  required_approvals: number
  base_set_version: number
  requested_by?: string | null
  status: WrpChangeStatus
  created_at: string
  expires_at: string
  decided_by?: string | null
  decided_at?: string | null
  decision_reason_code?: string | null
  executed_by?: string | null
  executed_at?: string | null
  approvals: WrpApproval[]
}

export interface WrpChangeResponse {
  change: WrpChange
  executed: boolean
  policy?: WrpVersion
  disclaimer: string
}

export function getWithdrawalRestrictionPolicy(tenantId: string): Promise<WrpEffective> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/withdrawal-restriction-policy`)
}

export function listWithdrawalRestrictionVersions(tenantId: string): Promise<{ items: WrpVersion[]; disclaimer: string }> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/withdrawal-restriction-policy/versions`)
}

export interface WrpChangeListParams {
  tenant_id?: string
  status?: WrpChangeStatus
  limit?: number
}

export function listWithdrawalRestrictionChanges(params: WrpChangeListParams = {}): Promise<{ items: WrpChange[]; disclaimer: string }> {
  return apiFetch(`/v1/admin/withdrawal-restriction-policy-changes${buildQuery(params)}`)
}

export function getWithdrawalRestrictionChange(changeId: string): Promise<WrpChangeResponse> {
  return apiFetch(`/v1/admin/withdrawal-restriction-policy-changes/${encodeURIComponent(changeId)}`)
}

export interface FileWrpChangeBody {
  tenant_id: string
  cause: WrpConfigurableCause
  action: WrpAction
  reason_code: string
  note?: string
}

export function fileWithdrawalRestrictionChange(body: FileWrpChangeBody): Promise<unknown> {
  return apiFetch('/v1/admin/withdrawal-restriction-policy-changes', { method: 'POST', body: JSON.stringify(body) })
}

export function approveWithdrawalRestrictionChange(changeId: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/withdrawal-restriction-policy-changes/${encodeURIComponent(changeId)}/approve`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }) })
}

export function rejectWithdrawalRestrictionChange(changeId: string, reasonCode: string): Promise<unknown> {
  return apiFetch(`/v1/admin/withdrawal-restriction-policy-changes/${encodeURIComponent(changeId)}/reject`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }) })
}

/** Requester only; no step-up and no request body. */
export function cancelWithdrawalRestrictionChange(changeId: string): Promise<unknown> {
  return apiFetch(`/v1/admin/withdrawal-restriction-policy-changes/${encodeURIComponent(changeId)}/cancel`, { method: 'POST' })
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

function asChangeResponse(body: unknown): WrpChangeResponse | null {
  if (!isObject(body) || !isObject(body.change) || typeof body.change.id !== 'string' || typeof body.change.status !== 'string' || typeof body.executed !== 'boolean') return null
  return body as unknown as WrpChangeResponse
}

export type WrpFilingResult = { kind: 'applied'; response: WrpChangeResponse } | { kind: 'pending'; response: WrpChangeResponse } | { kind: 'unconfirmed' }

/** `applied` = executed:true AND the change reads executed; `pending` = executed:false AND the change is still open (requested). */
export function classifyWrpFiling(body: unknown): WrpFilingResult {
  const r = asChangeResponse(body)
  if (!r) return { kind: 'unconfirmed' }
  if (r.executed === true && r.change.status === 'executed') return { kind: 'applied', response: r }
  if (r.executed === false && r.change.status === 'requested') return { kind: 'pending', response: r }
  return { kind: 'unconfirmed' }
}

export type WrpDecisionResult =
  | { kind: 'executed'; response: WrpChangeResponse }
  | { kind: 'recorded'; response: WrpChangeResponse }
  | { kind: 'rejected'; response: WrpChangeResponse }
  | { kind: 'cancelled'; response: WrpChangeResponse }
  | { kind: 'unconfirmed' }

export function classifyWrpDecision(op: 'approve' | 'reject' | 'cancel', body: unknown): WrpDecisionResult {
  const r = asChangeResponse(body)
  if (!r) return { kind: 'unconfirmed' }
  const s = r.change.status
  if (op === 'reject') return s === 'rejected' ? { kind: 'rejected', response: r } : { kind: 'unconfirmed' }
  if (op === 'cancel') return s === 'cancelled' ? { kind: 'cancelled', response: r } : { kind: 'unconfirmed' }
  if (r.executed === true && s === 'executed') return { kind: 'executed', response: r }
  if (r.executed === false && (s === 'requested' || s === 'approved')) return { kind: 'recorded', response: r }
  return { kind: 'unconfirmed' }
}

/** Strictness for the on-screen HINT only: allow < hold_for_review < block. The database classifies the change. */
export function actionRank(a: WrpAction): number {
  return WRP_ACTIONS.indexOf(a)
}
