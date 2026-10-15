// ADR 0114 S8 client: K3 payment force-resolutions (ADR 0101), M1 and M2 kinds only. M4 (evidence-backed) is
// slice S11 and is not offered. Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (PaymentForceResolutionRequest, PaymentForceResolution, ResolutionDecisionRequest,
// PaymentForceResolutionDecision).
//
// What the client must send (and nothing more):
//  - request: attempt_id, kind, reason_code, and per kind finding_code (M1) / basis_code + evidence_ref_hash (M2)
//    (+ optional context_code, note). `evidence_ref_hash` is a 64-char lowercase hex digest the OPERATOR supplies
//    (runbook: hex SHA-256 of the external evidence reference, computed outside the console). This module neither
//    hashes anything nor ever sees or sends the reference text.
//  - approve / reject: the `payload_hash` EXACTLY as the resolution DTO returned it, plus a reason_code. The hash
//    is computed and checked by the server; the client never derives it, only echoes the value it showed.
//  - the signed actor proof is produced server-side; no proof header or field exists on these routes.
//  - step-up: request and approve are `financial`; reject and cancel are `none` (golden table). The apiFetch
//    interceptor prompts and re-sends once.
import { apiFetch } from './client'
import { idempotencyHeaders } from './idempotency'

export const M1_KINDS = ['m1_deposit_evidence'] as const
export const M2_KINDS = ['m2_declare_paid', 'm2_declare_not_paid'] as const
export type ConsoleKind = (typeof M1_KINDS)[number] | (typeof M2_KINDS)[number]

export const FINDING_CODES = ['awaiting_psp_refund', 'refund_requested_from_psp', 'investigated_no_platform_action'] as const
export const BASIS_CODES = ['provider_confirmed_out_of_band', 'reconciliation_exhausted'] as const
export const CONTEXT_CODES = ['provider_unqueryable', 'past_resubmission_horizon'] as const

export const HEX64 = /^[0-9a-f]{64}$/

export interface ForceResolutionRequest {
  attempt_id: string
  kind: ConsoleKind
  finding_code?: string
  basis_code?: string
  context_code?: string
  evidence_ref_hash?: string
  reason_code: string
  note?: string
}

export interface PaymentForceResolution {
  id: string
  tenant_id: string
  attempt_id: string
  operation: string
  kind: string
  target_state?: string
  finding_code?: string
  basis_code?: string
  context_code?: string
  evidence_ref_hash?: string
  amount_minor_units: string
  asset_code: string
  reason_code: string
  /** The hash an approver must pin on approve or reject. */
  payload_hash: string
  /** Null for a tenant viewer when the requester is platform staff (H1). */
  requested_by: string | null
  requested_by_scope: string
  required_at_submission: number
  tenant_status_at_submission: string
  /** pending | executing | executed | rejected | cancelled | expired | refused_at_execution */
  state: string
  refusal_code?: string
  ledger_transaction_id?: string
  created_at: string
  expires_at: string
  evidence_line_id?: string
  evidence_reference_present?: boolean
  evidence_verdict?: string
  evidence_import_ids?: string[]
}

export interface ForceResolutionDecision {
  resolution: PaymentForceResolution
  executed: boolean
  refused: boolean
  counted_approvals: number
  required_approvals: number
}

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/payment-force-resolutions`

export function listForceResolutions(tenantId: string): Promise<{ items: PaymentForceResolution[] }> {
  return apiFetch(`${base(tenantId)}`)
}

export function getForceResolution(tenantId: string, id: string): Promise<PaymentForceResolution> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}`)
}

export function requestForceResolution(tenantId: string, body: ForceResolutionRequest, key?: string): Promise<PaymentForceResolution> {
  return apiFetch(`${base(tenantId)}`, { method: 'POST', body: JSON.stringify(body), headers: idempotencyHeaders(key) })
}

export function approveForceResolution(tenantId: string, id: string, payloadHash: string, reasonCode: string, key?: string): Promise<ForceResolutionDecision> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}/approve`, { method: 'POST', body: JSON.stringify({ payload_hash: payloadHash, reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

export function rejectForceResolution(tenantId: string, id: string, payloadHash: string, reasonCode: string, key?: string): Promise<ForceResolutionDecision> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}/reject`, { method: 'POST', body: JSON.stringify({ payload_hash: payloadHash, reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

/** No body. Only the original requester, only while pending (database-enforced). */
export function cancelForceResolution(tenantId: string, id: string, key?: string): Promise<PaymentForceResolution> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}/cancel`, { method: 'POST', headers: idempotencyHeaders(key) })
}
