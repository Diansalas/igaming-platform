// ADR 0116 s6 (A7) AML officer client. Wire shapes are copied from docs/api/openapi/platform-api.yaml (Aml* schemas,
// /v1/admin/tenants/{tenantID}/aml/*); no route or field is invented. Every function is a thin call: authorisation (the
// in-force aml_officer grant, the signed proof, the officer quorum, four-eyes distinctness, tenant scoping) is server-side
// and authoritative. The tenant is a PATH value only; no body ever carries a tenant or actor id. Step-up prompts happen in
// apiFetch (the StepUpProvider), never here. There is NO reject-proposal operation on the server (NOT IMPLEMENTED).
import { apiFetch, buildQuery } from './client'

export type HoldScope = 'all_value_movement' | 'withdrawal_and_payout'
export type HoldReasonClass = 'manual' | 'confirmed_sanctions'
export type CaseKind = 'manual' | 'alert'
export type CaseStatus =
  | 'open'
  | 'in_review'
  | 'escalated'
  | 'sar_decision_pending'
  | 'closed_no_action'
  | 'closed_sar_not_filed'
  | 'closed_sar_filed'
export type ProposalKind = 'close_sar_not_filed' | 'close_sar_filed' | 'lift_hold'
export type OwnerScope = 'platform' | 'tenant'

export interface AmlHold {
  id: string
  owner_scope: OwnerScope
  player_account_id?: string
  scope: HoldScope
  reason_class: HoldReasonClass
  reason_code: string
  placed_at: string
  lifted: boolean
  lifted_at?: string
}

export interface AmlQueueItem {
  case_id: string
  owner_scope: OwnerScope
  person_id: string
  status: CaseStatus
  sar_flagged: boolean
  last_seq: number
  opened_at: string
}

export interface AmlCase {
  id: string
  owner_scope: OwnerScope
  person_id: string
  kind: CaseKind
  status: CaseStatus
  sar_flagged: boolean
  /** The value every state-changing call must present as expected_seq. */
  last_seq: number
  opened_at: string
}

export interface AmlCaseEvent {
  seq: number
  type: string
  from_status?: string
  to_status?: string
  proposal_kind?: ProposalKind
  proposal_seq?: number
  content_hash?: string
  subject_hold_id?: string
  note_id?: string
  external_filing_reference?: string
  reason_code: string
  /** Null when redacted (ADR 0114 H1): a tenant-scoped caller does not see platform staff ids. */
  actor_id: string | null
  actor_scope: 'tenant' | 'platform_acting'
  created_at: string
}

export interface AmlCaseNote {
  id: string
  body: string
  lint_version: string
  /** Null when redacted (ADR 0114 H1). */
  created_by: string | null
  created_at: string
}

export interface AmlCaseDetail {
  case: AmlCase
  events: AmlCaseEvent[]
  notes: AmlCaseNote[]
}

/** Endpoint table: the single source of the paths this module calls. The contract test compares it with the OpenAPI document. */
export const AML_ENDPOINTS = {
  caseQueue: { method: 'GET', path: '/v1/admin/tenants/{tenantID}/aml/cases' },
  caseRead: { method: 'GET', path: '/v1/admin/tenants/{tenantID}/aml/cases/{caseID}' },
  personHolds: { method: 'GET', path: '/v1/admin/tenants/{tenantID}/aml/persons/{personID}/holds' },
  placeHold: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/holds' },
  liftHold: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/holds/{holdID}/lift' },
  openCase: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/cases' },
  transition: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/cases/{caseID}/transition' },
  addNote: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/cases/{caseID}/notes' },
  flagSar: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/cases/{caseID}/sar-flag' },
  propose: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/cases/{caseID}/proposals' },
  approve: { method: 'POST', path: '/v1/admin/tenants/{tenantID}/aml/cases/{caseID}/approvals' },
} as const

/** Fills a path template; every value is URL-encoded. Exported for the contract test. */
export function fillPath(template: string, params: Record<string, string>): string {
  return template.replace(/\{(\w+)\}/g, (_m, k: string) => encodeURIComponent(params[k] ?? ''))
}

const post = (path: string, body: unknown) => ({ method: 'POST', path, body: JSON.stringify(body) })

export function listCases(tenantId: string, limit?: number): Promise<{ items: AmlQueueItem[] }> {
  return apiFetch(fillPath(AML_ENDPOINTS.caseQueue.path, { tenantID: tenantId }) + buildQuery({ limit }))
}

export function getCase(tenantId: string, caseId: string): Promise<AmlCaseDetail> {
  return apiFetch(fillPath(AML_ENDPOINTS.caseRead.path, { tenantID: tenantId, caseID: caseId }))
}

export function listPersonHolds(tenantId: string, personId: string): Promise<{ items: AmlHold[] }> {
  return apiFetch(fillPath(AML_ENDPOINTS.personHolds.path, { tenantID: tenantId, personID: personId }))
}

export interface PlaceHoldBody {
  person_id: string
  player_account_id?: string | null
  scope: HoldScope
  reason_class: HoldReasonClass
  reason_code: string
}

export function placeHold(tenantId: string, body: PlaceHoldBody): Promise<{ hold_id: string }> {
  const r = post(fillPath(AML_ENDPOINTS.placeHold.path, { tenantID: tenantId }), body)
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export function liftHold(tenantId: string, holdId: string, reasonCode: string): Promise<{ lift_id: string }> {
  const r = post(fillPath(AML_ENDPOINTS.liftHold.path, { tenantID: tenantId, holdID: holdId }), { reason_code: reasonCode })
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export interface OpenCaseBody {
  person_id: string
  kind: CaseKind
  reason_code: string
  alert_ids?: string[]
}

export function openCase(tenantId: string, body: OpenCaseBody): Promise<{ case_id: string }> {
  const r = post(fillPath(AML_ENDPOINTS.openCase.path, { tenantID: tenantId }), body)
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export function transitionCase(tenantId: string, caseId: string, expectedSeq: number, to: CaseStatus, reasonCode: string): Promise<{ last_seq: number }> {
  const r = post(fillPath(AML_ENDPOINTS.transition.path, { tenantID: tenantId, caseID: caseId }), { expected_seq: expectedSeq, to, reason_code: reasonCode })
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export function addNote(tenantId: string, caseId: string, expectedSeq: number, body: string): Promise<{ last_seq: number }> {
  const r = post(fillPath(AML_ENDPOINTS.addNote.path, { tenantID: tenantId, caseID: caseId }), { expected_seq: expectedSeq, body })
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export function flagSar(tenantId: string, caseId: string, expectedSeq: number, reasonCode: string): Promise<{ last_seq: number }> {
  const r = post(fillPath(AML_ENDPOINTS.flagSar.path, { tenantID: tenantId, caseID: caseId }), { expected_seq: expectedSeq, reason_code: reasonCode })
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export interface ProposeBody {
  expected_seq: number
  kind: ProposalKind
  hold_id?: string
  external_filing_reference?: string
  reason_code: string
}

export function proposeAction(tenantId: string, caseId: string, body: ProposeBody): Promise<{ proposal_seq: number; content_hash: string }> {
  const r = post(fillPath(AML_ENDPOINTS.propose.path, { tenantID: tenantId, caseID: caseId }), body)
  return apiFetch(r.path, { method: r.method, body: r.body })
}

export function approveAction(tenantId: string, caseId: string, proposalSeq: number, contentHash: string, reasonCode: string): Promise<{ last_seq: number }> {
  const r = post(fillPath(AML_ENDPOINTS.approve.path, { tenantID: tenantId, caseID: caseId }), { proposal_seq: proposalSeq, content_hash: contentHash, reason_code: reasonCode })
  return apiFetch(r.path, { method: r.method, body: r.body })
}

/** The closed reason-code token the server accepts (^[a-z0-9_]{1,64}$). */
export const AML_REASON_CODE = /^[a-z0-9_]{1,64}$/
export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
