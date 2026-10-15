// ADR 0114 S3 client for the R1 read routes (payment attempts). Wire shapes are copied from
// docs/api/openapi/platform-api.yaml (PaymentAttempt, PaymentAttemptPage); no route or field is invented.
// Facts the UI relies on (all from the OpenAPI text):
//  - the response never carries the provider reference text (only `has_provider_reference`), the merchant
//    reference, a player/wallet id, a destination or a staff id; this module therefore has no such field and
//    the screens cannot render one;
//  - there is NO provider filter and NO age filter on the list (only operation, state, terminal_reason,
//    withdrawal_request_id) and NO per-attempt event history route; the keyset cursor is opaque.
import { apiFetch, buildQuery } from './client'

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
  limit?: number
  cursor?: string
}

const base = (tenantId: string) => `/v1/admin/tenants/${encodeURIComponent(tenantId)}/payment-attempts`

export function listPaymentAttempts(tenantId: string, params: ListAttemptsParams = {}): Promise<PaymentAttemptPage> {
  return apiFetch(`${base(tenantId)}${buildQuery(params)}`)
}

export function getPaymentAttempt(tenantId: string, attemptId: string): Promise<PaymentAttempt> {
  return apiFetch(`${base(tenantId)}/${encodeURIComponent(attemptId)}`)
}
