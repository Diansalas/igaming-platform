// Closed code map for every non-2xx on the S3/S5/S7/S8 routes (ADR 0114 invariant 7.9). The server `message` is
// NEVER displayed. The K3 and HSEC routes answer 403/404/409 with the generic envelope code plus a FIXED CLOSED
// TOKEN in `message` (OpenAPI ForceResolution* / HoldResolution* responses); that token is matched against the
// allow-list below and only the UI-owned text for a match is shown. An unknown message prints nothing from the
// server. Step-up outcomes are keyed on the client-generated code only.
import { STEP_UP_MESSAGES } from '../../auth/stepUpCoordinator'
import { ApiError } from '../../api/types'
import { safeCode } from '../killswitch/labels'

export type ConsoleFamily = 'attempts' | 'recon' | 'k1' | 'k3' | 'hsec' | 'staff'
export type ConsoleOp = 'list' | 'get' | 'request' | 'approve' | 'reject' | 'cancel' | 'revoke'

export interface ConsoleErrorView {
  message: string
  requestId?: string
  status?: number
  /** Only a safe token: either the envelope code or an allow-listed closed token. */
  code?: string
  /** True when what the user saw is stale, so the screen refreshes. */
  stale: boolean
}

const SAFE_CODE = /^[A-Za-z0-9_]{1,40}$/

const NOTHING = 'Nothing was changed.'

/** Closed tokens of the K3 routes (OpenAPI ForceResolutionForbidden / NotFound / Conflict). */
export const K3_TOKENS: Record<string, { status: number; text: string; stale: boolean }> = {
  force_resolve_not_permitted: {
    status: 403,
    text: `The server refused: you hold no in-force capability grant for this action on this tenant, a different person is required, or another authority rule applies. ${NOTHING}`,
    stale: false,
  },
  force_resolve_not_found: { status: 404, text: `This resolution or attempt does not exist for this tenant. ${NOTHING}`, stale: true },
  force_resolve_disabled: { status: 409, text: `Force-resolution is switched off for this tenant. ${NOTHING}`, stale: false },
  force_resolve_precondition_failed: {
    status: 409,
    text: `A precondition no longer holds (for example the attempt or its withdrawal changed state, or no statement source is registered for the attempt's provider). ${NOTHING}`,
    stale: true,
  },
  force_resolve_reason_not_resolvable: { status: 409, text: `This attempt's park reason cannot be resolved through this route. ${NOTHING}`, stale: true },
  force_resolve_conflict: {
    status: 409,
    text: `The resolution is not pending any more, another change won, or the database asked for a retry. ${NOTHING} The list is being refreshed.`,
    stale: true,
  },
  force_resolve_expired: { status: 409, text: `This resolution expired and can no longer be decided. ${NOTHING} The list is being refreshed.`, stale: true },
  force_resolve_evidence_insufficient: { status: 409, text: `The evidence is insufficient for this kind. ${NOTHING}`, stale: false },
  force_resolve_evidence_mismatch: { status: 409, text: `The evidence does not match the attempt. ${NOTHING}`, stale: false },
  force_resolve_evidence_overflow: { status: 409, text: `The evidence set is too large to evaluate. ${NOTHING}`, stale: false },
  force_resolve_evidence_unsealed: { status: 409, text: `The evidence is not sealed. ${NOTHING}`, stale: false },
}

/** Closed tokens of the HSEC routes (OpenAPI HoldResolutionForbidden / NotFound / Conflict). */
export const HSEC_TOKENS: Record<string, { status: number; text: string; stale: boolean }> = {
  hold_resolution_not_permitted: {
    status: 403,
    text: `The server refused: you hold no in-force platform grant or acting session for this tenant, a different person is required, or another authority rule applies. ${NOTHING}`,
    stale: false,
  },
  hold_resolution_not_found: { status: 404, text: `This resolution or withdrawal does not exist for this tenant. ${NOTHING}`, stale: true },
  hold_resolution_disabled: { status: 409, text: `Hold release is not enabled for this tenant (no enabled policy). ${NOTHING}`, stale: false },
  hold_resolution_precondition_failed: {
    status: 409,
    text: `A precondition does not hold (the withdrawal must be approved, with no payout attempt, on a tenant or brand that is not active). ${NOTHING}`,
    stale: true,
  },
  hold_resolution_conflict: {
    status: 409,
    text: `The resolution is not pending any more, another change won, or the database asked for a retry. ${NOTHING} The list is being refreshed.`,
    stale: true,
  },
  hold_resolution_expired: { status: 409, text: `This resolution expired and can no longer be decided. ${NOTHING} The list is being refreshed.`, stale: true },
}

const FORBIDDEN = `The server refused this: your account does not hold the permission for this action, or it may not act on this tenant. ${NOTHING}`

function stepUp(err: ApiError): string | null {
  if (err.code === 'step_up_cancelled') return STEP_UP_MESSAGES.cancelled
  if (err.code === 'step_up_failed') return STEP_UP_MESSAGES.locked
  if (err.code === 'step_up_required') return STEP_UP_MESSAGES.not_effective
  return null
}

/** Replaces the "nothing was changed" assurance, which is NOT true after a re-send whose first send may have committed. */
const RETRIED = 'This dialog already sent this action once. The first send may have reached the server and may already be recorded, so check the list before trying again.'

export function describeConsoleError(err: unknown, family: ConsoleFamily, op: ConsoleOp, opts: { retried?: boolean } = {}): ConsoleErrorView {
  const v = describeConsoleErrorBase(err, family, op)
  if (!opts.retried || !(err instanceof ApiError) || err.status < 400) return v
  // A network failure or a 5xx on a re-send says nothing was confirmed already; a definite refusal after a first
  // send is exactly the case where the first send may have committed.
  const message = v.message.split(NOTHING).join(RETRIED)
  return { ...v, message: message === v.message ? `${v.message} ${RETRIED}` : message, stale: true }
}

function describeConsoleErrorBase(err: unknown, family: ConsoleFamily, op: ConsoleOp): ConsoleErrorView {
  if (!(err instanceof ApiError)) return { message: 'Something went wrong. Nothing was confirmed. Please try again.', stale: false }
  const base = { requestId: err.requestId, status: err.status, code: SAFE_CODE.test(err.code) ? err.code : undefined, stale: false }
  if (err.code === 'network_error') return { ...base, message: 'Could not reach the server. Nothing was confirmed. Check your connection and try again.' }
  const su = stepUp(err)
  if (su) return { ...base, message: su }
  if (err.code === 'session_revoked') return { ...base, message: 'Your session has ended. Sign in again.' }
  const table = family === 'k3' ? K3_TOKENS : family === 'hsec' ? HSEC_TOKENS : null
  if (table && Object.prototype.hasOwnProperty.call(table, err.message) && table[err.message].status === err.status) {
    const t = table[err.message]
    return { ...base, code: err.message, stale: t.stale, message: t.text }
  }
  switch (err.status) {
    case 401:
      return { ...base, message: 'Your session has ended. Sign in again.' }
    case 403:
      return { ...base, message: FORBIDDEN }
    case 404:
      if (op === 'list') return { ...base, message: `The server could not find this tenant. ${NOTHING}` }
      return { ...base, stale: true, message: `This item no longer exists for this tenant. ${NOTHING} The list is being refreshed.` }
    case 409:
      if (family === 'k1') {
        if (op === 'request')
          return { ...base, stale: true, message: `The server refused the request. Common causes: a platform session may not request a grant for a tenant staff member (the tenant administrator requests it), a duplicate request, or a database guard. ${NOTHING}` }
        if (op === 'approve' || op === 'reject')
          return { ...base, stale: true, message: `The server refused the decision. Common causes: the request is no longer pending, it expired, or a database rule such as the distinct-person rule applies. ${NOTHING} The list is being refreshed.` }
        if (op === 'cancel') return { ...base, stale: true, message: `The server refused the cancellation: only the person who filed the request may cancel it, and only while it is pending. ${NOTHING}` }
        if (op === 'revoke') return { ...base, stale: true, message: `The server refused: the grant is already revoked. ${NOTHING} The list is being refreshed.` }
      }
      return { ...base, stale: true, message: `The server reported a conflict. ${NOTHING} The list is being refreshed.` }
    case 400:
      return { ...base, message: `The server rejected the input as invalid. Check every field against its stated format. ${NOTHING}` }
    case 429:
      return { ...base, message: 'Too many requests. Wait a moment and try again.' }
    case 503:
      return { ...base, message: `The service is temporarily unavailable. Nothing was confirmed. Try again shortly.` }
  }
  if (err.status >= 500) return { ...base, message: 'The server could not complete the request. Nothing was confirmed. Try again, and quote the request ID if it persists.' }
  return { ...base, message: 'The request could not be completed. Nothing was confirmed.' }
}

export function refusalIsStale(err: unknown, family: ConsoleFamily, op: ConsoleOp): boolean {
  return describeConsoleError(err, family, op).stale
}

/** A closed-set refusal code returned in a 200 body (refused_at_execution); anything unknown prints as a safe token only. */
export const REFUSAL_TEXT: Record<string, string> = {
  no_statement_source: 'no statement source is registered for the provider',
  withdrawal_not_approved: 'the withdrawal is no longer approved',
  attempt_exists: 'a payout attempt now exists',
  tenant_brand_active_again: 'the tenant and brand are active again',
  policy_disabled: 'the hold-release policy is disabled',
}

export function describeRefusalCode(code: string | undefined): string {
  if (!code) return 'a precondition no longer held'
  return Object.prototype.hasOwnProperty.call(REFUSAL_TEXT, code) ? REFUSAL_TEXT[code] : `a precondition no longer held (${safeCode(code)})`
}
