// lic31: AML screening POLICY client (lic29 routes GET/POST /v1/admin/tenants/{tenantID}/screening/policy). Wire shapes are copied
// from docs/api/openapi/platform-api.yaml (AMLScreeningPolicy, AMLScreeningPolicyRequest, AMLScreeningRefusalCode). Facts the UI
// relies on, all enforced by the SERVER (this module only sends and classifies):
//  - the read needs aml:case:read; the write needs aml:hold:manage AND step-up `critical` (apiFetch prompts and re-sends once),
//    a platform_acting AML officer, the owner-scope officer quorum and a signed actor proof (ADR 0110, produced server-side);
//  - the platform ships NO re-screen interval and NO grace: every field of the request is a human decision, nothing is defaulted;
//  - a version is TIGHTEN-ONLY (no list removed, no longer interval or grace, no earlier effective time) and loosening is not
//    available over HTTP (409 policy_loosening_refused);
//  - the tenant is a PATH value only. No body carries a tenant, actor or proof.
import { apiFetch, buildQuery } from './client'
import { SCREENING_LISTS, UUID_RE, type ScreeningList } from './amlScreening'

/** Engineering safety floor (lic30), not a legal value: below it every clear expires within the hour and withdrawals freeze. */
export const RESCREEN_INTERVAL_FLOOR_SECONDS = 3600
export const RESCREEN_INTERVAL_MAX_SECONDS = 31536000
export const BACKFILL_GRACE_MAX_SECONDS = 31536000
/** Effective time window: now (2 minutes of clock skew) to 30 days ahead. */
export const EFFECTIVE_PAST_TOLERANCE_MS = 2 * 60_000
export const EFFECTIVE_MAX_AHEAD_MS = 30 * 86_400_000

export interface ScreeningPolicy {
  configured: boolean
  lists: ScreeningList[]
  rescreen_interval_seconds: number | null
  backfill_grace_seconds: number | null
  effective_from: string | null
}

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const isList = (v: unknown): v is ScreeningList => typeof v === 'string' && (SCREENING_LISTS as readonly string[]).includes(v)
const optInt = (v: unknown): boolean => v === null || v === undefined || (typeof v === 'number' && Number.isInteger(v) && v >= 0)

/** Returns the policy when the body has the documented shape, otherwise null (the caller shows "unrecognised", never a guess). */
export function parseScreeningPolicy(body: unknown): ScreeningPolicy | null {
  if (!isObject(body) || typeof body.configured !== 'boolean') return null
  if (!Array.isArray(body.lists) || !body.lists.every(isList)) return null
  if (!optInt(body.rescreen_interval_seconds) || !optInt(body.backfill_grace_seconds)) return null
  const from = body.effective_from
  if (from != null && typeof from !== 'string') return null
  return {
    configured: body.configured,
    lists: body.lists,
    rescreen_interval_seconds: (body.rescreen_interval_seconds as number | null | undefined) ?? null,
    backfill_grace_seconds: (body.backfill_grace_seconds as number | null | undefined) ?? null,
    effective_from: (from as string | null | undefined) ?? null,
  }
}

export function readScreeningPolicy(tenantId: string, jurisdictionId: string): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/screening/policy${buildQuery({ jurisdiction_id: jurisdictionId })}`)
}

export interface SetScreeningPolicyBody {
  jurisdiction_id: string
  lists: ScreeningList[]
  rescreen_interval_seconds: number
  backfill_grace_seconds: number
  effective_from: string
  legal_review_reference: string
  reason_code: string
}

export function setScreeningPolicy(tenantId: string, body: SetScreeningPolicyBody): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/screening/policy`, { method: 'POST', body: JSON.stringify(body) })
}

export type SetScreeningPolicyResult = { kind: 'confirmed'; policyId: string } | { kind: 'unconfirmed' }
/** A 201 is confirmed only by a body carrying a policy_id (uuid); anything else is not a confirmation. */
export function classifySetScreeningPolicy(body: unknown): SetScreeningPolicyResult {
  if (isObject(body) && typeof body.policy_id === 'string' && UUID_RE.test(body.policy_id)) return { kind: 'confirmed', policyId: body.policy_id }
  return { kind: 'unconfirmed' }
}
