// ADR 0114 S7 client: K1 capability grants (ADR 0099). Wire shapes are copied from
// docs/api/openapi/platform-api.yaml (CapabilityGrantRequest, CapabilityGrant, *Body); no field is invented.
// API facts the UI relies on:
//  - list routes are bare arrays, no pagination, no filters;
//  - K1 has NO payload_hash: approve/reject take only { reason_code }. The "proof-bound payload hash" of K2/K3/
//    HSEC does not exist on K1 (the signed actor proof is produced server-side, ADR 0110), so none is shown;
//  - step-up: request, approve and revoke are `critical` (the apiFetch interceptor prompts and re-sends once);
//    reject and cancel are `none`;
//  - requested_by / grantee_staff_id are null for a tenant viewer when the actor is platform staff (ADR 0114 H1).
import { apiFetch, buildQuery } from './client'
import { idempotencyHeaders } from './idempotency'

export const GRANT_CAPABILITIES = [
  'ledger_adjustment:initiate',
  'ledger_adjustment:approve',
  'payment_force_resolve:request',
  'payment_force_resolve:approve',
  'withdrawal_hold_resolution:request',
  'withdrawal_hold_resolution:approve',
] as const
export type GrantCapability = (typeof GRANT_CAPABILITIES)[number]

export interface GrantRequestBody {
  grantee_staff_id: string
  capability: GrantCapability
  valid_from?: string
  valid_until?: string
  reason_code: string
}

export interface CapabilityGrantRequest {
  id: string
  tenant_id: string
  grantee_staff_id: string | null
  grantee_scope: string
  capability: string
  valid_from: string
  valid_until?: string
  reason_code: string
  requested_by: string | null
  requested_by_scope: string
  /** pending | approved | rejected | cancelled | expired */
  status: string
  created_at: string
  expires_at: string
}

export interface CapabilityGrant {
  id: string
  tenant_id: string
  grantee_staff_id: string | null
  grantee_scope: string
  capability: string
  valid_from: string
  valid_until?: string
  granted_at: string
  revoked_at?: string
  revoke_reason_code?: string
}

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/capability-grants`

export function listGrantRequests(tenantId: string): Promise<CapabilityGrantRequest[]> {
  return apiFetch(`${base(tenantId)}/requests`)
}

export function listGrants(tenantId: string): Promise<CapabilityGrant[]> {
  return apiFetch(`${base(tenantId)}`)
}

export function createGrantRequest(tenantId: string, body: GrantRequestBody, key?: string): Promise<CapabilityGrantRequest> {
  return apiFetch(`${base(tenantId)}/requests`, { method: 'POST', body: JSON.stringify(body), headers: idempotencyHeaders(key) })
}

export function approveGrantRequest(tenantId: string, requestId: string, reasonCode: string, key?: string): Promise<CapabilityGrant> {
  return apiFetch(`${base(tenantId)}/requests/${encodeURIComponent(requestId)}/approve`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

/** 204 on success. */
export function rejectGrantRequest(tenantId: string, requestId: string, reasonCode: string, key?: string): Promise<void> {
  return apiFetch(`${base(tenantId)}/requests/${encodeURIComponent(requestId)}/reject`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

/** 204 on success; no body. */
export function cancelGrantRequest(tenantId: string, requestId: string, key?: string): Promise<void> {
  return apiFetch(`${base(tenantId)}/requests/${encodeURIComponent(requestId)}/cancel`, { method: 'POST', headers: idempotencyHeaders(key) })
}

/** 204 on success. The emergency stop: a single actor, no second approver. */
export function revokeGrant(tenantId: string, grantId: string, reasonCode: string, key?: string): Promise<void> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(grantId)}/revoke`, { method: 'POST', body: JSON.stringify({ reason_code: reasonCode }), headers: idempotencyHeaders(key) })
}

// --- staff directory (grantee picker). ADR 0119 section 14.2: staff:read; never lists platform staff. ---
export interface StaffDirectoryEntry {
  id: string
  display_name: string | null
  role: string
  status: string
  person_linked: boolean
}

export function listTenantStaff(tenantId: string, params: { status?: string; role?: string } = {}): Promise<{ staff: StaffDirectoryEntry[]; truncated: boolean }> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/staff${buildQuery(params)}`)
}
