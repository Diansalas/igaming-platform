// UI-owned vocabulary for the parked payments console (ADR 0114 S3). Every enum falls back to "Unrecognised" plus a
// safe token; no server text is rendered. Wording states what the control does; it makes no legal or regulatory claim.
import type { BadgeTone } from '../../components/Badge'
import { safeCode, type EnumView } from '../killswitch/labels'

function view(map: Record<string, string>, v: unknown): EnumView {
  if (typeof v === 'string' && Object.prototype.hasOwnProperty.call(map, v)) return { label: map[v], known: true }
  return { label: 'Unrecognised', code: safeCode(v), known: false }
}

export const describeAttemptState = (v: unknown) =>
  view(
    {
      created: 'Created',
      submitting: 'Submitting',
      pending: 'Pending at provider',
      ambiguous: 'Ambiguous (outcome unknown)',
      succeeded: 'Succeeded',
      declined: 'Declined',
      rejected: 'Rejected by platform',
      disputed: 'Parked (disputed)',
    },
    v,
  )

export function attemptStateTone(v: unknown): BadgeTone {
  if (v === 'disputed') return 'danger'
  if (v === 'ambiguous') return 'warning'
  if (v === 'succeeded') return 'success'
  if (v === 'pending' || v === 'submitting' || v === 'created') return 'info'
  return 'neutral'
}

export const describeOperation = (v: unknown) => view({ payout: 'Payout', deposit: 'Deposit' }, v)

const REASONS: Record<string, string> = {
  amount_asset_mismatch: 'Amount or asset differs between withdrawal and attempt',
  callback_amount_asset_mismatch: 'Provider callback amount or asset differs',
  destination_integrity_failure: 'Payout destination failed an integrity check',
  destination_mismatch: 'Payout destination differs from the bound instrument',
  expired_before_submission: 'Expired before it was submitted',
  intent_succeeded: 'Intent already succeeded elsewhere',
  invalid_provider_reference: 'Provider reference rejected',
  'invalid_provider_reference:control_char': 'Provider reference rejected: control character',
  'invalid_provider_reference:empty': 'Provider reference rejected: empty',
  'invalid_provider_reference:invalid_utf8': 'Provider reference rejected: invalid text encoding',
  'invalid_provider_reference:reserved_namespace': 'Provider reference rejected: reserved namespace',
  'invalid_provider_reference:too_long': 'Provider reference rejected: too long',
  kill_switch: 'Blocked by a payment kill switch',
  late_contradicting_evidence: 'Late evidence contradicts the recorded outcome',
  late_decline_after_terminal: 'Late decline after the attempt was final',
  late_success_after_terminal: 'Late success after the attempt was final',
  multiple_success_for_intent: 'More than one success for one intent',
  no_routable_provider: 'No provider could be routed',
  poll_amount_mismatch: 'Status poll amount differs',
  poll_reference_mismatch: 'Status poll reference differs',
  provider_reference_conflict: 'Provider reference conflicts with another attempt',
  provider_reference_mismatch: 'Provider reference differs from the one held',
  reversal_tombstone_precedes_success: 'A reversal was recorded before the success arrived',
  success_after_payout_declined: 'Success reported after the payout was declined',
  success_for_never_sent_attempt: 'Success reported for an attempt that was never sent',
  sync_amount_mismatch: 'Synchronous response amount differs',
  other: 'Other (not in the closed catalogue)',
}

export function describeReason(v: unknown): EnumView {
  if (v === null || v === undefined) return { label: 'None', known: true }
  return view(REASONS, v)
}

export const KIND_LABELS: Record<string, string> = {
  m1_deposit_evidence: 'M1 - record evidence on a disputed deposit',
  m2_declare_paid: 'M2 - declare the payout paid',
  m2_declare_not_paid: 'M2 - declare the payout not paid',
  m4_evidence_paid: 'M4 - statement evidence: paid',
  m4_evidence_not_paid: 'M4 - statement evidence: not paid',
}

export const describeKind = (v: unknown) => view(KIND_LABELS, v)

/** M4 is slice S11; this console lists M4 kinds as a hint only and offers no M4 action. */
export const CONSOLE_KINDS = ['m1_deposit_evidence', 'm2_declare_paid', 'm2_declare_not_paid'] as const

export const ADMISSIBLE_HINT =
  'This list is a hint computed by the server now. The request path re-checks in the database, and a platform session cannot see whether a resolution is already open.'
