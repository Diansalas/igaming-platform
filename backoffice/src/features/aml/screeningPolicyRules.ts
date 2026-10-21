// Client-side mirror of the lic29 / lic30 screening-policy rules (internal/aml, migrations 0181/0189). They only save a round trip:
// the server is authoritative and every refusal is rendered. Whole seconds only; integers are read from digit-only text with
// JSON.parse, so there is no float arithmetic anywhere.
import {
  BACKFILL_GRACE_MAX_SECONDS,
  EFFECTIVE_MAX_AHEAD_MS,
  EFFECTIVE_PAST_TOLERANCE_MS,
  RESCREEN_INTERVAL_FLOOR_SECONDS,
  RESCREEN_INTERVAL_MAX_SECONDS,
  type ScreeningPolicy,
} from '../../api/amlScreeningPolicy'
import type { ScreeningList } from '../../api/amlScreening'

const WHOLE = /^(0|[1-9][0-9]{0,9})$/

/** Exact integer from digit-only text, or null. Leading zeros, signs, decimals and exponents are not accepted. */
export function wholeSeconds(text: string): number | null {
  return WHOLE.test(text) ? (JSON.parse(text) as number) : null
}

export function intervalProblem(text: string): string | null {
  if (text === '') return 'This is required.'
  const n = wholeSeconds(text)
  if (n === null) return 'Use whole seconds, digits only.'
  if (n < RESCREEN_INTERVAL_FLOOR_SECONDS) return `The interval is below the engineering safety floor of ${RESCREEN_INTERVAL_FLOOR_SECONDS} seconds.`
  if (n > RESCREEN_INTERVAL_MAX_SECONDS) return `The interval is above the maximum of ${RESCREEN_INTERVAL_MAX_SECONDS} seconds.`
  return null
}

export function graceProblem(text: string): string | null {
  if (text === '') return 'This is required (0 is a stated value).'
  const n = wholeSeconds(text)
  if (n === null) return 'Use whole seconds, digits only.'
  if (n > BACKFILL_GRACE_MAX_SECONDS) return `The grace is above the maximum of ${BACKFILL_GRACE_MAX_SECONDS} seconds.`
  return null
}

/** True when `ms` lies outside now-2min .. now+30d. A hint; the server compares against the database clock. */
export function effectiveOutsideWindow(ms: number, now: number = Date.now()): boolean {
  return ms < now - EFFECTIVE_PAST_TOLERANCE_MS || ms > now + EFFECTIVE_MAX_AHEAD_MS
}

/** Twice the gate alert threshold (integer seconds): the smallest interval the server accepts where a gate is in force. */
export const minIntervalForGate = (gateAlertSeconds: number): number => gateAlertSeconds * 2

export interface PolicyDraft {
  lists: ScreeningList[]
  interval: number
  grace: number
  effectiveMs: number
}

/**
 * Ways in which the draft is certainly LOOSER than the policy in force (the server refuses a version looser than the version in
 * force and than the latest version). Empty when nothing is in force. Fixed words only.
 */
export function looseningReasons(inForce: ScreeningPolicy | null, d: PolicyDraft): string[] {
  if (!inForce || !inForce.configured) return []
  const out: string[] = []
  const removed = inForce.lists.filter((l) => !d.lists.includes(l))
  if (removed.length > 0) out.push('a list screened by the policy in force would be removed')
  if (inForce.rescreen_interval_seconds !== null && d.interval > inForce.rescreen_interval_seconds) out.push('the re-screen interval would be longer than in the policy in force')
  if (inForce.backfill_grace_seconds !== null && d.grace > inForce.backfill_grace_seconds) out.push('the backfill grace would be longer than in the policy in force')
  if (inForce.effective_from !== null) {
    const t = Date.parse(inForce.effective_from)
    if (!Number.isNaN(t) && d.effectiveMs < t) out.push('the effective time would be earlier than that of the policy in force')
  }
  return out
}
