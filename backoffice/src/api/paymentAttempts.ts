// ADR 0114 S3 client for the R1 read routes (payment attempts). Wire shapes are copied from
// docs/api/openapi/platform-api.yaml (PaymentAttempt, PaymentAttemptPage); no route or field is invented.
// Facts the UI relies on (all from the OpenAPI text):
//  - the response never carries the provider reference text (only `has_provider_reference`), the merchant
//    reference, a player/wallet id, a destination or a staff id; this module therefore has no such field and
//    the screens cannot render one;
//  - A10c: the list also filters by provider_id (exact) and min_age_seconds (at least that old), and a per-attempt
//    event history route lists the verified provider receipts (closed vocabularies only, never a reference text);
//    the keyset cursor is opaque.
import { apiFetch, buildQuery } from './client'
import type { PagedResponse } from './types'

export const ATTEMPT_OPERATIONS = ['payout', 'deposit'] as const
export type AttemptOperation = (typeof ATTEMPT_OPERATIONS)[number]

export const ATTEMPT_STATES = ['created', 'submitting', 'pending', 'ambiguous', 'succeeded', 'declined', 'rejected', 'disputed'] as const
export type AttemptState = (typeof ATTEMPT_STATES)[number]

/** The closed input vocabulary of the `terminal_reason` filter (`other` is output-only). */
export const TERMINAL_REASONS = [
  'amount_asset_mismatch',
  'callback_amount_asset_mismatch',
  'destination_integrity_failure',
  'destination_mismatch',
  'expired_before_submission',
  'intent_succeeded',
  'invalid_provider_reference',
  'invalid_provider_reference:control_char',
  'invalid_provider_reference:empty',
  'invalid_provider_reference:invalid_utf8',
  'invalid_provider_reference:reserved_namespace',
  'invalid_provider_reference:too_long',
  'kill_switch',
  'late_contradicting_evidence',
  'late_decline_after_terminal',
  'late_success_after_terminal',
  'multiple_success_for_intent',
  'no_routable_provider',
  'poll_amount_mismatch',
  'poll_reference_mismatch',
  'provider_reference_conflict',
  'provider_reference_mismatch',
  'reversal_tombstone_precedes_success',
  'success_after_payout_declined',
  'success_for_never_sent_attempt',
  'sync_amount_mismatch',
] as const

export interface PaymentAttempt {
  id: string
  operation: AttemptOperation
  attempt_no: number
  /** Enum on the wire; an unknown value is rendered as Unrecognised, never trusted. */
  state: string
  /** A catalogue value, `other`, or null. */
  terminal_reason: string | null
  /** Exactly one property is present. Never a player or wallet identity. */
  subject_ref: { withdrawal_request_id?: string; deposit_intent_id?: string }
  /** Decimal string of minor units. */
  amount_minor_units: string
  asset_code: string
  exponent: number
  provider_id: string | null
  has_provider_reference: boolean
  created_at: string
  updated_at: string
  resolved_at: string | null
  /** Always null for a platform caller (see the OpenAPI description). */
  open_resolution_id: string | null
  admissible_kinds: string[]
}

export interface PaymentAttemptPage {
  items: PaymentAttempt[]
  next_cursor: string | null
}

export interface ListAttemptsParams {
  operation?: AttemptOperation
  state?: AttemptState
  terminal_reason?: string
  withdrawal_request_id?: string
  /** Exact provider id (^[a-z0-9][a-z0-9-]{0,62}$). */
  provider_id?: string
  /** Only attempts created at least this many seconds ago (0..31536000). */
  min_age_seconds?: number
  limit?: number
  cursor?: string
}

/** One verified provider callback receipt linked to an attempt (PaymentAttemptEvent). */
export interface AttemptEvent {
  id: string
  event_type: string
  outcome: string
  disposition_at_receipt: string
  resolution: string | null
  decline_stage: string | null
  amount_minor_units: string | null
  asset_code: string | null
  received_at: string
  resolved_at: string | null
}

export const PROVIDER_ID_RE = /^[a-z0-9][a-z0-9-]{0,62}$/

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/payment-attempts`

export function listPaymentAttempts(tenantId: string, params: ListAttemptsParams = {}): Promise<PaymentAttemptPage> {
  return apiFetch(`${base(tenantId)}${buildQuery(params)}`)
}

export function getPaymentAttempt(tenantId: string, attemptId: string): Promise<PaymentAttempt> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(attemptId)}`)
}

export function listAttemptEvents(tenantId: string, attemptId: string, params: { limit?: number; offset?: number } = {}): Promise<PagedResponse<AttemptEvent>> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(attemptId)}/events${buildQuery(params)}`)
}
