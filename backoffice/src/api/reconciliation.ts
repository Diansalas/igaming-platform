// ADR 0114 S5 client: reconciliation runs and mismatches. Two route families, both read-only:
//  - R2 (`/v1/admin/tenants/{tenantID}/reconciliation/{runs,mismatches}?stream=`): payment_statement,
//    ledger_vs_projection, sportsbook_settlement. The stored reconciliation key and the expected/actual diagnostic
//    texts are NEVER returned; amounts appear only for pay_amount_mismatch / pay_asset_mismatch.
//  - the existing casino routes (`/v1/admin/casino/reconciliation/{runs,mismatches}`): tenant-scoped (no tenant in
//    the path; a platform token gets no data) and they DO return reconciliation_key / expected_value /
//    actual_value free text, which the screen renders as plain text only.
// There is no single-run route, no single-mismatch route, no run_id filter on mismatches, no severity field on the
// R2 shapes and no mutation route: nothing here resolves or compensates a finding.
import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export const STAFF_STREAMS = ['payment_statement', 'ledger_vs_projection', 'sportsbook_settlement'] as const
export type StaffStream = (typeof STAFF_STREAMS)[number]
export const CASINO_STREAMS = ['casino_consistency', 'casino_statement'] as const
export type CasinoStream = (typeof CASINO_STREAMS)[number]

export const RECON_STATUSES = ['open', 'investigating', 'resolved'] as const

/** Closed kind vocabulary per stream (StaffReconciliationKind). */
export const KINDS_BY_STREAM: Record<StaffStream, readonly string[]> = {
  ledger_vs_projection: ['missing_projection', 'balance_mismatch', 'ledger_unlinked_manual_adjustment'],
  payment_statement: [
    'pay_missing_platform_record',
    'pay_missing_provider_record',
    'pay_amount_mismatch',
    'pay_asset_mismatch',
    'pay_reference_mismatch',
    'pay_status_mismatch',
    'pay_duplicate',
    'pay_unresolved',
    'pay_captured_unposted',
    'pay_declared_paid_unconfirmed',
    'pay_declared_not_paid_but_paid',
    'pay_declared_paid_compensated_but_paid',
  ],
  sportsbook_settlement: ['sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger', 'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch'],
}

export interface StaffReconRun {
  id: string
  stream: string
  period_start: string
  period_end: string
  run_at: string
  /** clean | mismatches_found | other */
  status: string
}

export interface StaffReconMismatch {
  id: string
  run_id: string
  stream: string
  /** A kind of the stream, or `other`. */
  kind: string
  /** open | investigating | resolved | other */
  status: string
  subject_ref: { type: string; id: string } | null
  expected_minor_units: string | null
  actual_minor_units: string | null
  asset_code: string | null
  exponent: number | null
  detected_at: string
}

export interface CasinoReconRun {
  id: string
  stream: string
  status: string
  period_start: string
  period_end: string
  run_at: string
}

export interface CasinoReconMismatch {
  id: string
  run_id: string
  mismatch_kind: string
  reconciliation_key: string
  expected_value: string
  actual_value: string
  investigation_status: string
  created_at: string
  resolved_at: string | null
}

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/reconciliation`

export interface PageParams {
  limit?: number
  offset?: number
}

export function listStaffReconRuns(tenantId: string, params: { stream: StaffStream } & PageParams): Promise<PagedResponse<StaffReconRun>> {
  return apiFetch(`${base(tenantId)}/runs${buildQuery(params)}`)
}

export interface StaffMismatchParams extends PageParams {
  stream: StaffStream
  status?: string
  kind?: string
  subject_id?: string
}

export function listStaffReconMismatches(tenantId: string, params: StaffMismatchParams): Promise<PagedResponse<StaffReconMismatch>> {
  return apiFetch(`${base(tenantId)}/mismatches${buildQuery(params)}`)
}

export function listCasinoReconRuns(params: { stream?: CasinoStream } & PageParams = {}): Promise<PagedResponse<CasinoReconRun>> {
  return apiFetch(`/v1/admin/casino/reconciliation/runs${buildQuery(params)}`)
}

export function listCasinoReconMismatches(params: { stream?: CasinoStream; status?: string } & PageParams = {}): Promise<PagedResponse<CasinoReconMismatch>> {
  return apiFetch(`/v1/admin/casino/reconciliation/mismatches${buildQuery(params)}`)
}
