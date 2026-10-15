// Maps staff-lifecycle API failures (ADR 0119 S5/S6 contract section 1, error table) to fixed, non-leaky messages.
// The UI branches on `code` ONLY and never renders the server's `message` text, so a wording change or a message that
// accidentally carries detail can never reach the screen. The request id is kept for support correlation.
import { ApiError } from '../../api/types'
import type { ErrorDetails } from '../../lib/apiErrorMessage'
import { STEP_UP_MESSAGES } from '../../auth/stepUpCoordinator'

export const INVITE_FAILED = 'The invitation could not be sent. Nothing was changed.'

export function describeInviteError(err: unknown): ErrorDetails {
  if (!(err instanceof ApiError)) return { message: INVITE_FAILED }
  const requestId = err.requestId
  const m = (message: string): ErrorDetails => ({ message, requestId })
  switch (err.code) {
    case 'validation_error':
      return m('Check the email address, the role and the Person ID. The Person must already exist.')
    case 'role_not_grantable':
      return m('Your account may not grant this role. finance, risk_manager, promotions_manager and bonus_operations are granted by a platform administrator only.')
    case 'person_required':
      return m('This role requires a linked Person. Enter the Person ID.')
    case 'conflict':
      // One code for two refusals (email already used in this tenant / Person already linked to a live account):
      // say both, neither can be told apart from the code and the UI never parses the server text.
      return m('A staff account already exists for this email in this tenant, or this Person is already linked to a live staff account. Use a different email or Person.')
    case 'self_target_forbidden':
      return m('You cannot perform this action on your own account.')
    case 'four_eyes_violation':
      return m('Four-eyes approval failed: the approver must be a different staff member and a different Person.')
    case 'lifecycle_busy':
      return m('The staff record is busy. Wait a moment and try again.')
    case 'lifecycle_transition_invalid':
      return m('The staff member is not in a state this action applies to.')
    case 'last_admin':
      return m('This change would remove the last active administrator.')
    case 'forbidden':
      return m('You do not have permission to invite staff for this tenant.')
    case 'not_found':
      return m('This tenant was not found, or is not available to you.')
    case 'session_revoked':
      return m(STEP_UP_MESSAGES.session_ended)
    case 'step_up_cancelled':
      return m('Verification cancelled. The invitation was not sent.')
    case 'step_up_failed':
    case 'step_up_required':
      // Fixed code-keyed text from the step-up flow itself (never server text).
      return m(err.message === STEP_UP_MESSAGES.locked ? STEP_UP_MESSAGES.locked : STEP_UP_MESSAGES.not_effective)
    case 'unauthorized':
      return m('Your session has ended. Sign in again.')
    case 'rate_limited':
      return m(err.retryAfterSeconds ? `Too many requests. Wait ${err.retryAfterSeconds} seconds and try again.` : 'Too many requests. Wait a minute and try again.')
    case 'network_error':
      return m('Could not reach the server. Check your connection and try again. The invitation may not have been sent.')
    case 'service_unavailable':
      return m('Staff administration is temporarily unavailable. Try again shortly.')
    default:
      return m(INVITE_FAILED)
  }
}

/** Fixed wording for the list read. */
export function describeStaffListError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.code === 'forbidden') return 'You do not have permission to view this tenant’s staff.'
    if (err.code === 'not_found') return 'This tenant was not found, or is not available to you.'
    if (err.code === 'network_error') return 'Could not reach the server.'
    if (err.code === 'session_revoked') return STEP_UP_MESSAGES.session_ended
  }
  return 'The staff list could not be loaded.'
}
