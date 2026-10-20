// lic20: AML screening trigger policy and officer resolution client (lic18 routes). Wire shapes are copied from
// docs/api/openapi/platform-api.yaml (AMLScreeningTriggerPolicy, AMLScreeningTriggerPolicyRequest, AMLScreeningRequest,
// AMLScreeningResolutionRequest). Facts the UI relies on, all enforced by the SERVER (this module only sends and classifies):
//  - reads need aml:case:read, writes aml:hold:manage AND step-up `critical` (apiFetch prompts and re-sends once);
//  - authoring a trigger policy is for a platform_acting AML officer only; resolving a request is the owner-scope officer's;
//  - the platform ships NO trigger policy, NO interval and NO grace; the request carries only fields a human chose;
//  - a withdrawal gate is refused unless a screening worker heartbeat exists (the production binary starts no worker);
//  - the tenant is a PATH value only. No body carries a tenant, actor or proof: the signed actor proof is server-side.
import { apiFetch, buildQuery } from './client'

export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
export const TRIGGER_EVENTS = ['registration', 'kyc_approval', 'scheduled'] as const
export type TriggerEvent = (typeof TRIGGER_EVENTS)[number]
export const SCREENING_LISTS = ['sanctions', 'pep'] as const
export type ScreeningList = (typeof SCREENING_LISTS)[number]
export type ResolutionOutcome = 'clear' | 'potential_match' | 'confirmed'
export const GATE_ALERT_MIN_SECONDS = 60
export const GATE_ALERT_MAX_SECONDS = 2592000

export interface TriggerPolicy {
  configured: boolean
  events: TriggerEvent[]
  gate_withdrawal: boolean
  gate_alert_after_seconds: number | null
  effective_from: string | null
}

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const isEvent = (v: unknown): v is TriggerEvent => typeof v === 'string' && (TRIGGER_EVENTS as readonly string[]).includes(v)

/** Returns the policy when the body has the documented shape, otherwise null (the caller shows "unrecognised", never a guess). */
export function parseTriggerPolicy(body: unknown): TriggerPolicy | null {
  if (!isObject(body)) return null
  if (typeof body.configured !== 'boolean' || typeof body.gate_withdrawal !== 'boolean') return null
  if (!Array.isArray(body.events) || !body.events.every(isEvent)) return null
  const secs = body.gate_alert_after_seconds
  if (secs != null && !(typeof secs === 'number' && Number.isInteger(secs))) return null
  const from = body.effective_from
  if (from != null && typeof from !== 'string') return null
  return { configured: body.configured, events: body.events, gate_withdrawal: body.gate_withdrawal, gate_alert_after_seconds: (secs as number | null | undefined) ?? null, effective_from: (from as string | null | undefined) ?? null }
}

export async function readTriggerPolicy(tenantId: string, jurisdictionId: string): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/screening/trigger-policy${buildQuery({ jurisdiction_id: jurisdictionId })}`)
}

export interface SetTriggerPolicyBody {
  jurisdiction_id: string
  events: TriggerEvent[]
  gate_withdrawal: boolean
  /** Present iff gate_withdrawal (the server refuses it without the gate). */
  gate_alert_after_seconds?: number
  effective_from: string
  legal_review_reference: string
  reason_code: string
}

export function setTriggerPolicy(tenantId: string, body: SetTriggerPolicyBody): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/screening/trigger-policy`, { method: 'POST', body: JSON.stringify(body) })
}

export type SetPolicyResult = { kind: 'confirmed'; policyId: string } | { kind: 'unconfirmed' }
/** A 201 is confirmed only by a body carrying a policy_id; anything else is not a confirmation. */
export function classifySetPolicy(body: unknown): SetPolicyResult {
  if (isObject(body) && typeof body.policy_id === 'string' && UUID_RE.test(body.policy_id)) return { kind: 'confirmed', policyId: body.policy_id }
  return { kind: 'unconfirmed' }
}

export type RequestStatus = 'pending' | 'claimed' | 'done' | 'needs_officer' | 'failed_terminal'
export interface ScreeningRequest {
  id: string
  trigger: string
  idempotency_key: string
  status: string
  attempts: number
  outcome?: string
  match_lists?: string[]
  last_error_code?: string
  completed_at?: string
  resolved: boolean
  created_at: string
}

export async function listScreeningRequests(tenantId: string, personId: string): Promise<unknown> {
  return apiFetch(`/v1/admin/tenants/${encodeURIComponent(tenantId)}/screening/persons/${encodeURIComponent(personId)}/requests`)
}

/** `{items: [...]}` with every item of the documented shape, else null (shown as unrecognised, never partially trusted). */
export function parseRequests(body: unknown): ScreeningRequest[] | null {
  if (!isObject(body) || !Array.isArray(body.items)) return null
  const out: ScreeningRequest[] = []
  for (const it of body.items) {
    if (!isObject(it)) return null
    if (typeof it.id !== 'string' || typeof it.trigger !== 'string' || typeof it.idempotency_key !== 'string' || typeof it.status !== 'string') return null
    if (typeof it.attempts !== 'number' || typeof it.resolved !== 'boolean' || typeof it.created_at !== 'string') return null
    out.push(it as unknown as ScreeningRequest)
  }
  return out
}

export interface ResolveBody {
  outcome: ResolutionOutcome
  lists: ScreeningList[]
  /** Only for potential_match / confirmed. */
  match_lists?: ScreeningList[]
  reason_code: string
}

export function resolveScreeningRequest(tenantId: string, personId: string, requestId: string, body: ResolveBody): Promise<unknown> {
  return apiFetch(
    `/v1/admin/tenants/${encodeURIComponent(tenantId)}/screening/persons/${encodeURIComponent(personId)}/requests/${encodeURIComponent(requestId)}/resolution`,
    { method: 'POST', body: JSON.stringify(body) },
  )
}

export type ResolveResult = { kind: 'confirmed'; resultId: string; replayed: boolean; holdId: string | null } | { kind: 'unconfirmed' }
/** A 201 is confirmed only by result_id (uuid) and a boolean replayed; hold_id, when present, must be a uuid. */
export function classifyResolution(body: unknown): ResolveResult {
  if (!isObject(body) || typeof body.result_id !== 'string' || !UUID_RE.test(body.result_id) || typeof body.replayed !== 'boolean') return { kind: 'unconfirmed' }
  if (body.hold_id != null && !(typeof body.hold_id === 'string' && UUID_RE.test(body.hold_id))) return { kind: 'unconfirmed' }
  return { kind: 'confirmed', resultId: body.result_id, replayed: body.replayed, holdId: (body.hold_id as string | undefined) ?? null }
}
