// Maps every launch-route failure to a fixed, UI-owned sentence. The server's `message` field is
// NEVER displayed (no raw error text leakage, ADR 0112 section 9); only the closed `code`, the HTTP
// status and the closed `check_codes` vocabulary are used. No sentence states a legal conclusion.
import { ApiError, LAUNCH_ERROR_CODES, type LaunchErrorCode } from '../../api/types'
import { CHECK_TITLES } from './labels'

export const LAUNCH_ERROR_MESSAGES: Record<LaunchErrorCode | 'TENANT_CLOSE_BLOCKED_OPEN_ROUNDS', string> = {
  LAUNCH_PRECONDITION_FAILED: 'A hard precondition for this action is not met. Fix the items listed below and try again.',
  LAUNCH_UNACKNOWLEDGED_CHECKS: 'Every readiness check that is not passing must be acknowledged. Tick each one and try again.',
  LAUNCH_REQUEST_PENDING: 'A request for this subject is already pending. Decide or cancel it before filing another.',
  LAUNCH_SELF_APPROVAL: 'You cannot decide a request you filed, and the same person cannot decide it from another account. A different platform approver is needed.',
  LAUNCH_ALREADY_DECIDED: 'You already decided this request. Nothing was changed.',
  LAUNCH_ILLEGAL_TRANSITION: 'This action is not allowed from the current status (for example, the subject may already be suspended).',
  LAUNCH_SUBJECT_CLOSED: 'This subject is closed. Closure is terminal and it cannot be changed.',
  LAUNCH_WITHDRAWALS_HOLDING_FUNDS: 'Closure refused: withdrawal requests still hold player funds. Resolve them first. The request stays pending.',
  LAUNCH_OPEN_BETS_BLOCK_CLOSURE: 'Closure refused: open sportsbook bets exist for this brand. Resolve them first. The request stays pending.',
  LAUNCH_RETRYABLE_LOCK_TIMEOUT: 'The subject is busy right now. Nothing was recorded. Try again in a moment.',
  LAUNCH_REQUEST_NOT_PENDING: 'This request is no longer pending (it was decided, executed, cancelled, expired or superseded). Refresh to see its current state.',
  LAUNCH_STALE_READINESS: 'The readiness changed since you opened this request. No approval was recorded. Review the refreshed checks and decide again.',
  LAUNCH_PRIOR_REJECT: 'An identical request was rejected recently and cannot be filed again within the request window. Change the request or wait.',
  LAUNCH_PRIOR_REJECTER: 'You rejected an earlier variant of this request. Approving needs a change note on the request.',
  LAUNCH_NOT_LEGACY_BASELINE: 'Only a subject whose status comes from the legacy baseline can be ratified.',
  LAUNCH_REFUSED_AT_EXECUTION: 'Your decision was recorded, but execution was refused. The status did not change. See the reference below.',
  LAUNCH_GUARD_REFUSED: 'A database safeguard refused this action. Nothing was changed.',
  LAUNCH_PROOF_REFUSED: 'The signed proof for this action was refused. Nothing was changed. Contact platform support with the request ID.',
  LAUNCH_NO_PERSON: 'Your account is not linked to a person record, so the four-eyes rule cannot be checked. Ask an administrator to link it.',
  LAUNCH_AUTHORISATION_UNAVAILABLE: 'The launch authorisation service is not available right now. Nothing was read or written.',
  TENANT_CLOSE_BLOCKED_OPEN_ROUNDS: 'Closure refused: open sportsbook bets exist. Resolve them first. The request stays pending.',
}

const KNOWN = new Set<string>([...LAUNCH_ERROR_CODES, 'TENANT_CLOSE_BLOCKED_OPEN_ROUNDS'])

/** Closed check-code vocabulary: H-n:..., R-..., refusal codes and SQLSTATEs. */
const CHECK_CODE_TEXT: Record<string, string> = {
  'H-1': 'Subject integrity or illegal transition',
  'H-2': 'The recorded licensing basis contradicts the registry',
  'H-3': 'At least one jurisdiction with a configuration in effect is required',
  'H-4': 'Four-eyes requirement not met',
  'H-5': 'A check that is not passing was not acknowledged',
  'H-6': 'Withdrawal requests still hold player funds',
  approvals_stale: 'An approval is too old',
  readiness_changed_since_approval: 'Readiness changed after the approval was given',
  readiness_changed_unacknowledged: 'A check that is now failing was not acknowledged',
  LA023: 'Withdrawal requests still hold player funds',
  LA024: 'Open sportsbook bets exist',
}
const SAFE_CODE = /^[A-Za-z0-9][A-Za-z0-9:_.-]{0,63}$/

export function describeCheckCode(code: string): string {
  const base = code.split(':')[0]
  return CHECK_CODE_TEXT[code] ?? CHECK_CODE_TEXT[base] ?? CHECK_TITLES[base] ?? 'Reference code'
}

export interface LaunchErrorView {
  message: string
  requestId?: string
  /** Closed codes only (filtered to a safe charset), each with fixed text. */
  checkCodes: { code: string; text: string }[]
  code?: string
  status?: number
}

export function describeLaunchError(err: unknown): LaunchErrorView {
  if (!(err instanceof ApiError)) {
    return { message: 'Something went wrong. Nothing was confirmed. Please try again.', checkCodes: [] }
  }
  const checkCodes = err.checkCodes.filter((c) => SAFE_CODE.test(c)).map((code) => ({ code, text: describeCheckCode(code) }))
  const base = { requestId: err.requestId, checkCodes, code: err.code as string, status: err.status }
  if (KNOWN.has(err.code)) {
    return { ...base, message: LAUNCH_ERROR_MESSAGES[err.code as keyof typeof LAUNCH_ERROR_MESSAGES] }
  }
  switch (err.code) {
    case 'network_error':
      return { ...base, message: 'Could not reach the server. Check your connection and try again.' }
    case 'unauthorized':
      return { ...base, message: 'Your session has ended. Sign in again.' }
    case 'forbidden':
      return { ...base, message: 'You are not allowed to do this. The server refused the action; nothing was changed.' }
    case 'not_found':
      return { ...base, message: 'Not found. It may not exist, or it may belong to another tenant.' }
    case 'validation_error':
    case 'invalid_request':
      return { ...base, message: 'The server rejected the request as invalid. Check the fields (reason code, ids, note) and try again.' }
    case 'service_unavailable':
      return { ...base, message: 'The service is not available right now. Try again later.' }
    default:
      if (err.status === 403) return { ...base, message: 'You are not allowed to do this. The server refused the action; nothing was changed.' }
      if (err.status === 404) return { ...base, message: 'Not found. It may not exist, or it may belong to another tenant.' }
      return { ...base, message: 'The action could not be completed. Nothing was confirmed. Please try again, and quote the request ID if it persists.' }
  }
}
