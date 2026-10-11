// Fixed UI-owned vocabulary for the alerts console (ADR 0114 S1). Nothing here is taken from server text:
// the server's `kind` is an open string, so every unknown value falls back to a fixed sentence plus the code.
import type { BadgeTone } from '../../components/Badge'

/** Always shown, plainly: ALERT-DELIVERY-1 is OPEN, so no alert reaches a person. */
export const DELIVERY_BANNER_TITLE = 'MOCK / no delivery channel configured'
export const DELIVERY_BANNER_BODY =
  'ALERT-DELIVERY-1 is OPEN: alerts are raised and stored here, but nobody is paged, e-mailed or messaged. ' +
  'Do not assume anyone has been told about an alert. This console is the only place they are visible.'

export interface KindInfo {
  title: string
  description: string
}

/** Kind catalogue mirrored from internal/alerting/kind.go. Fixed sentences; no legal conclusions. */
export const KIND_CATALOGUE: Record<string, KindInfo> = {
  'alerting.unrouted': { title: 'Alert had no route', description: 'An alert was raised but no delivery route exists for its severity.' },
  'alerting.delivery_dead': { title: 'Alert delivery gave up', description: 'Delivery of an alert failed repeatedly and was abandoned.' },
  'alerting.raise_failed': { title: 'Alert could not be recorded', description: 'The platform failed to record an alert it tried to raise.' },
  'payment.multiple_success_for_intent': { title: 'Several successes for one deposit', description: 'More than one successful outcome was recorded for a single deposit intent.' },
  'payment.deposit_intent_index_backstop_fired': { title: 'Deposit duplicate safeguard fired', description: 'A database safeguard against duplicate deposit intents refused a write.' },
  'reconciliation.ledger_projection_drift': { title: 'Ledger and balance projection differ', description: 'A recomputed balance does not match the stored projection.' },
  'reconciliation.sportsbook_settlement_mismatch': { title: 'Sportsbook settlement mismatch', description: 'Sportsbook settlement does not match the ledger.' },
  'reconciliation.casino_consistency_mismatch': { title: 'Casino consistency mismatch', description: 'Casino round records are inconsistent with the ledger.' },
  'reconciliation.casino_statement_mismatch': { title: 'Casino statement mismatch', description: 'A casino provider statement does not match the ledger.' },
  'reconciliation.payment_statement_mismatch': { title: 'Payment statement mismatch', description: 'A payment provider statement does not match the ledger.' },
  'reconciliation.run_failed': { title: 'Reconciliation run failed', description: 'A scheduled reconciliation run did not complete.' },
  'payment.kill_switch_engaged': { title: 'Payment kill switch engaged', description: 'A payment kill switch was engaged.' },
  'casino.callback_integrity': { title: 'Casino callback integrity failure', description: 'A casino provider callback failed an integrity check.' },
  'payment.webhook_integrity': { title: 'Payment webhook integrity failure', description: 'A payment webhook failed an integrity check.' },
  'simulation.payment.payload_mismatch': { title: 'Simulation: payment payload mismatch', description: 'A simulated payment callback did not match its payload. Simulation only.' },
  'simulation.casino_play': { title: 'Simulation: casino play', description: 'A casino simulation raised an alert. Simulation only.' },
  'simulation.sportsbook_settlement': { title: 'Simulation: sportsbook settlement', description: 'A sportsbook simulation raised an alert. Simulation only.' },
  'kyc.submission_failed_terminal': { title: 'KYC submission failed permanently', description: 'A KYC submission to the provider reached a terminal failure.' },
  'launch.suspension_exhausted': { title: 'Suspension could not be executed', description: 'Every attempt to execute a launch suspension failed, so a subject that should be suspended may not be.' },
}

export const UNKNOWN_KIND_TITLE = 'Unrecognised alert kind'

const SAFE_KIND = /^[a-z0-9][a-z0-9_.-]{0,80}$/

export function describeKind(kind: string): { title: string; description: string; code: string; known: boolean } {
  const info = Object.prototype.hasOwnProperty.call(KIND_CATALOGUE, kind) ? KIND_CATALOGUE[kind] : undefined
  if (info) return { ...info, code: kind, known: true }
  return {
    title: UNKNOWN_KIND_TITLE,
    description: 'This console does not know this kind. Only its code is shown.',
    code: SAFE_KIND.test(kind) ? kind : 'unprintable',
    known: false,
  }
}

export const STATE_OPTIONS = [
  { value: 'open', label: 'Open' },
  { value: 'acked', label: 'Acknowledged' },
  { value: 'resolved', label: 'Resolved' },
]
export const SEVERITY_OPTIONS = [
  { value: 'p1', label: 'P1' },
  { value: 'p2', label: 'P2' },
  { value: 'p3', label: 'P3' },
]
export const LIMIT_OPTIONS = [
  { value: '50', label: '50 newest' },
  { value: '100', label: '100 newest' },
  { value: '200', label: '200 newest' },
]

const STATE_LABEL: Record<string, string> = { open: 'Open', acked: 'Acknowledged', resolved: 'Resolved' }
export function stateLabel(s: string): string {
  return STATE_LABEL[s] ?? 'Unknown state'
}
export function stateTone(s: string): BadgeTone {
  return s === 'open' ? 'danger' : s === 'acked' ? 'warning' : s === 'resolved' ? 'success' : 'neutral'
}
export function severityLabel(s: string): string {
  return s === 'p1' || s === 'p2' || s === 'p3' ? s.toUpperCase() : 'Unknown'
}
export function severityTone(s: string): BadgeTone {
  return s === 'p1' ? 'danger' : s === 'p2' ? 'warning' : 'neutral'
}

const DELIVERY_LABEL: Record<string, string> = {
  pending: 'Delivery pending',
  retrying: 'Delivery retrying',
  unrouted: 'Not routed anywhere',
  delivery_failed: 'Delivery failed',
  suppressed: 'Suppressed',
  recorded_non_human: 'Recorded to a log/mock sink only',
  delivered: 'Delivered to a person',
}
export function deliveryLabel(s: string): string {
  return DELIVERY_LABEL[s] ?? 'Unknown delivery state'
}
const UNROUTED_LABEL: Record<string, string> = {
  no_route: 'no route is configured',
  channel_disabled: 'the route is disabled',
  no_sink: 'no sink exists',
}
export function unroutedLabel(r: string | null | undefined): string | undefined {
  return r ? (UNROUTED_LABEL[r] ?? 'unknown reason') : undefined
}

export const REASON_HELP =
  'A lowercase token of up to 64 characters (a-z, 0-9, _ . -), for example "investigated". It is recorded in the audit log. Not free text: do not enter names or personal data.'
