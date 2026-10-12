// ADR 0114 S6 payment kill switch client. Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (/v1/admin/tenants/{tenantID}/payments/kill-switches*, kill-switch-release-requests*); no route or field is
// invented. API facts the UI relies on: there is NO platform-wide or provider-wide route (everything is under a
// tenant path), NO list of release requests (a pending request is found through the switch's
// `release_request_id`), NO per-change history route (the list returns one current row per scope pair, released
// rows included) and NO provider registry route.
import { apiFetch } from './client'

export type KillSwitchOperation = 'deposit' | 'payout' | '*'

/** Per ADR 0114 section 9 the actor ids are null for a tenant viewer when the actor is platform staff. */
export interface KillSwitch {
  id: string
  tenant_id: string
  /** '*' or a provider id. */
  provider_scope: string
  /** Enum deposit | payout | * on the wire; unknown values are rendered as Unrecognised. */
  operation_scope: string
  engaged: boolean
  engaged_by_scope?: string | null
  release_request_id?: string | null
  reason_code: string
  changed_by: string | null
  changed_by_scope: string
  changed_at: string
  version: number
  created_at: string
}

export interface KillSwitchReleaseRequest {
  id: string
  tenant_id: string
  kill_switch_id: string
  expected_version: number
  reason_code: string
  requested_by: string | null
  requested_by_scope: string
  created_at: string
  expires_at: string
  status: string
  approved_by?: string | null
  approved_by_scope?: string | null
  decided_at?: string | null
}

export const KILL_SWITCH_REASON_MAX = 64

export interface EngageRequest {
  provider_scope: string
  operation_scope: KillSwitchOperation
  reason_code: string
}

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/payments`

export function listKillSwitches(tenantId: string): Promise<{ kill_switches: KillSwitch[] }> {
  return apiFetch(`${base(tenantId)}/kill-switches`)
}

export function engageKillSwitch(tenantId: string, body: EngageRequest): Promise<KillSwitch> {
  return apiFetch(`${base(tenantId)}/kill-switches`, { method: 'POST', body: JSON.stringify(body) })
}

export function requestRelease(tenantId: string, killSwitchId: string, reasonCode: string): Promise<KillSwitchReleaseRequest> {
  return apiFetch(`${base(tenantId)}/kill-switches/${encodeURIComponent(killSwitchId)}/release-requests`, {
    method: 'POST',
    body: JSON.stringify({ reason_code: reasonCode }),
  })
}

export function getReleaseRequest(tenantId: string, requestId: string): Promise<KillSwitchReleaseRequest> {
  return apiFetch(`${base(tenantId)}/kill-switch-release-requests/${encodeURIComponent(requestId)}`)
}

/** Approve and release in one server transaction; returns the switch after release. */
export function approveRelease(tenantId: string, requestId: string): Promise<KillSwitch> {
  return apiFetch(`${base(tenantId)}/kill-switch-release-requests/${encodeURIComponent(requestId)}/approve`, { method: 'POST' })
}

/** 204 on success. */
export function cancelRelease(tenantId: string, requestId: string): Promise<void> {
  return apiFetch(`${base(tenantId)}/kill-switch-release-requests/${encodeURIComponent(requestId)}/cancel`, { method: 'POST' })
}
