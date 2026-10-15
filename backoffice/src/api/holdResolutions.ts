// ADR 0114 S8 / ADR 0111 section 6.3 (HSEC) client: withdrawal hold-resolutions. platform_admin ONLY (no tenant
// role holds these permissions). Wire shapes from docs/api/openapi/platform-api.yaml
// (WithdrawalHoldResolutionRequest, WithdrawalHoldResolution, HoldResolutionDecisionRequest,
// WithdrawalHoldResolutionDecision). Same contract as K3: the client echoes the `payload_hash` it displayed, the
// operator supplies the 64-hex `evidence_ref_hash` (computed outside the console), the actor proof is server-side,
// request and approve are step-up `financial`, reject and cancel are `none`.
// Known API gap (ADR 0114 R3): there is no candidate-list route, so the withdrawal id is typed by the operator;
// nothing here can look a withdrawal up.
import { apiFetch } from './client'
import { idempotencyHeaders } from './idempotency'

export interface HoldResolutionRequest {
  withdrawal_request_id: string
  kind: 'release_hold_to_player'
  /** ^[a-z][a-z0-9_]{0,63}$ */
  reason_code: string
  evidence_ref_hash: string
  note?: string
}

export interface WithdrawalHoldResolution {
  id: string
  tenant_id: string
  withdrawal_request_id: string
  kind: string
  amount_minor_units: string
  asset_code: string
  reason_code: string
  evidence_ref_hash: string
  payload_hash: string
  requested_by: string | null
  requested_by_scope: string
  required_at_submission: number
  tenant_status_at_submission: string
  brand_status_at_submission: string
  state: string
  refusal_code?: string
  ledger_transaction_id?: string
  created_at: string
  expires_at: string
}

export interface HoldResolutionDecision {
  resolution: WithdrawalHoldResolution
  executed: boolean
  refused: boolean
  counted_approvals: number
  required_approvals: number
}

export const HOLD_REASON_RE = /^[a-z][a-z0-9_]{0,63}$/

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/withdrawal-hold-resolutions`

export function listHoldResolutions(tenantId: string): Promise<{ items: WithdrawalHoldResolution[] }> {
  return apiFetch(`${base(tenantId)}`)
}

export function getHoldResolution(tenantId: string, id: string): Promise<WithdrawalHoldResolution> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}`)
}

export function requestHoldResolution(tenantId: string, body: HoldResolutionRequest, key?: string): Promise<WithdrawalHoldResolution> {
  return apiFetch(`${base(tenantId)}`, { method: 'POST', body: JSON.stringify(body), headers: idempotencyHeaders(key) })
}

export function approveHoldResolution(tenantId: string, id: string, payloadHash: string, reasonCode: string, key?: string): Promise<HoldResolutionDecision> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}/approve`, { method: 'POST', body: JSON.stringify({ payload_hash: payloadHash, reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

export function rejectHoldResolution(tenantId: string, id: string, payloadHash: string, reasonCode: string, key?: string): Promise<HoldResolutionDecision> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}/reject`, { method: 'POST', body: JSON.stringify({ payload_hash: payloadHash, reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

export function cancelHoldResolution(tenantId: string, id: string, key?: string): Promise<WithdrawalHoldResolution> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(id)}/cancel`, { method: 'POST', headers: idempotencyHeaders(key) })
}
