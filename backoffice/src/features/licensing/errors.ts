// Maps every licensing-route failure to a fixed, UI-owned sentence (the server `message` is NEVER shown). The closed
// `code`, the HTTP status and the closed LC0xx check codes (when the server sends them) are the only server text used.
import { ApiError, LICENSING_CHANGE_ERROR_CODES } from '../../api/types'
import { STEP_UP_MESSAGES } from '../../auth/stepUpCoordinator'
import { safeToken } from './labels'

const NOTHING = 'Nothing was changed.'

export const LICENSING_ERROR_MESSAGES: Record<(typeof LICENSING_CHANGE_ERROR_CODES)[number], string> = {
  LICENSING_CHANGE_SELF_APPROVAL: `You cannot decide a request you filed, and another account of the same person cannot either. A different person must decide it. ${NOTHING}`,
  LICENSING_CHANGE_NOT_PENDING: `This request is no longer open (it was executed, rejected, cancelled or expired). Refresh to see its current state. ${NOTHING}`,
  LICENSING_CHANGE_ALREADY_DECIDED: `You (this person) already approved this request. A second approval must come from a different person. ${NOTHING}`,
  LICENSING_CHANGE_DUPLICATE_PENDING: `An identical request is already open. Approve or cancel that one instead of filing another. ${NOTHING}`,
  LICENSING_CHANGE_APPROVALS_STALE: `An earlier approval is older than 24 hours and no longer counts. Cancel this request and file a new one. ${NOTHING}`,
  LICENSING_CHANGE_REFUSED: `The change was refused by the domain rules at execution (for example a precondition such as a country ceiling or a licensee mismatch). Fix the precondition and file a new request. ${NOTHING}`,
  LICENSING_CHANGE_GUARD_REFUSED: `A database safeguard (an LC0xx guard) refused the change. Often the change would now widen and a new request is needed. ${NOTHING}`,
  LICENSING_CHANGE_RETRYABLE_LOCK_TIMEOUT: `The record is busy right now. ${NOTHING} Try again in a moment.`,
  LICENSING_CHANGE_PROOF_REFUSED: `The signed proof for this action was refused. ${NOTHING} Contact platform support with the request ID.`,
  LICENSING_CHANGE_NO_PERSON: `Your account is not linked to a person record, so the two-person rule cannot be checked. ${NOTHING} Ask an administrator to link it.`,
  LICENSING_CHANGE_UNAVAILABLE: `The licensing change service is not available in this environment. ${NOTHING}`,
}

/** Meaning of each LC0xx guard code, from docs/runbooks/licensing-change-approvals.md and migrations 0177/0178. */
export const LC_CODE_TEXT: Record<string, string> = {
  LC001: 'This session may not act on licensing changes',
  LC002: 'A tenant session may not request this kind of change',
  LC010: 'The payload or reason is invalid, or names an unknown record',
  LC011: 'The change is not valid for the current state',
  LC012: 'A request cannot be edited after it is filed',
  LC013: 'This status move is not allowed (or the request expired)',
  LC020: 'The approvals are too old',
  LC021: 'Not enough approvals',
  LC022: 'The requester (or their person) may not decide',
  LC023: 'Only the requester may cancel or execute this',
  LC024: 'The change would now widen: file a new request',
  LC030: 'Only an eligible platform administrator may decide',
  LC031: 'The request is not open for approval',
  LC032: 'The requester may not approve (two-person rule)',
  LC040: 'Governed data changes only through an executed request',
  LC041: 'The tenant jurisdiction configuration is append-only',
  LC042: 'Licence identity fields can never change',
  LC043: 'The content is not valid for the current records (for example a licensee or kind mismatch, a licence bound to another tenant, or the basis changed since filing)',
}
const LC_CODE = /^LC0\d\d$/

export interface LicensingErrorView {
  message: string
  code?: string
  status?: number
  requestId?: string
  lcCodes: { code: string; text: string }[]
}

export function describeLicensingError(err: unknown): LicensingErrorView {
  if (!(err instanceof ApiError)) return { message: 'Something went wrong. Nothing was confirmed. Please try again.', lcCodes: [] }
  const lcCodes = err.checkCodes.filter((c) => LC_CODE.test(c)).map((code) => ({ code, text: LC_CODE_TEXT[code] ?? 'Reference code' }))
  const base = { code: err.code as string, status: err.status, requestId: err.requestId, lcCodes }
  if (Object.prototype.hasOwnProperty.call(LICENSING_ERROR_MESSAGES, err.code)) {
    return { ...base, message: LICENSING_ERROR_MESSAGES[err.code as keyof typeof LICENSING_ERROR_MESSAGES] }
  }
  switch (err.code) {
    case 'step_up_cancelled':
      return { ...base, message: STEP_UP_MESSAGES.cancelled }
    case 'step_up_failed':
      return { ...base, message: STEP_UP_MESSAGES.locked }
    case 'step_up_required':
      return { ...base, message: STEP_UP_MESSAGES.not_effective }
    case 'network_error':
      return { ...base, message: 'Could not reach the server. Nothing was confirmed. Check your connection and try again.' }
    case 'unauthorized':
      return { ...base, message: 'Your session has ended. Sign in again.' }
    case 'forbidden':
      return { ...base, message: `The server refused this: your account does not hold the permission for this action. ${NOTHING}` }
    case 'not_found':
      return { ...base, message: 'Not found. It may not exist, or you may not be allowed to see it.' }
    case 'validation_error':
    case 'invalid_request':
      return { ...base, message: `The server rejected the request as invalid (for example a precondition such as an unknown id, a licensee that does not match the tenant's licensing model, or a value that changes nothing). Check the fields. ${NOTHING}` }
    case 'service_unavailable':
      return { ...base, message: `The service is not available right now. Nothing was confirmed. Try again later.` }
  }
  if (err.status === 403) return { ...base, message: `The server refused this: your account does not hold the permission for this action. ${NOTHING}` }
  if (err.status === 404) return { ...base, message: 'Not found. It may not exist, or you may not be allowed to see it.' }
  return { ...base, message: 'The action could not be completed. Nothing was confirmed. Check the changes list before trying again, and quote the request ID if it persists.' }
}
