// Maps staff-auth API failures (ADR 0119 S3 contract section 2) to fixed,
// accessible messages. The UI branches on `code` ONLY and never renders the
// server's `message` text, so a server-side wording change or a message that
// accidentally carries detail can never become an oracle in the UI.
import { ApiError } from '../api/types'

/** Where the flow goes after a failure. */
export type FlowEffect =
  /** Stay on the current step, let the user retry. */
  | 'stay'
  /** The challenge is dead (expired/used/exhausted/locked): back to the password step. */
  | 'restart'
  /** The pending factor expired: call enrol/start again with the same challenge. */
  | 'restart_enrolment'

export interface StaffAuthFailure {
  message: string
  effect: FlowEffect
}

export type StaffAuthStep = 'login' | 'mfa' | 'enrol' | 'redeem'

export const GENERIC_FAILURE = 'Sign-in failed. Please try again.'
export const UNAVAILABLE = 'Sign-in is temporarily unavailable. Try again.'
export const CHALLENGE_EXPIRED = 'Your sign-in session expired. Please sign in again.'

export function describeStaffAuthError(err: unknown, step: StaffAuthStep): StaffAuthFailure {
  if (!(err instanceof ApiError)) return { message: GENERIC_FAILURE, effect: 'stay' }
  switch (err.code) {
    case 'network_error':
      return { message: 'Could not reach the server. Check your connection and try again.', effect: 'stay' }
    case 'service_unavailable':
      // One fixed message whatever the verification outcome would have been (gate G4).
      return { message: UNAVAILABLE, effect: 'stay' }
    case 'rate_limited': {
      const wait = err.retryAfterSeconds
      return {
        message: wait ? `Too many attempts. Wait ${wait} seconds and try again.` : 'Too many attempts. Wait a minute.',
        effect: 'stay',
      }
    }
    case 'validation_error':
      return { message: 'Please check the details you entered and try again.', effect: 'stay' }
    case 'unauthorized':
      // Only the password step answers a bare 401 `unauthorized`.
      return step === 'login'
        ? { message: 'Email or password is incorrect.', effect: 'stay' }
        : { message: CHALLENGE_EXPIRED, effect: 'restart' }
    case 'not_found':
      return { message: 'Unknown operator.', effect: 'stay' }
    case 'forbidden':
      return { message: 'This account cannot log in. Contact your administrator.', effect: step === 'login' ? 'stay' : 'restart' }
    case 'mfa_enrolment_required':
      return {
        message: 'Your account has no authenticator yet. Ask your administrator for an enrolment link.',
        effect: 'stay',
      }
    case 'mfa_locked':
      return {
        message:
          'Too many incorrect codes. Try again in 15 minutes, or ask your administrator to reset your authenticator.',
        effect: step === 'login' ? 'stay' : 'restart',
      }
    case 'invalid_challenge':
      return { message: CHALLENGE_EXPIRED, effect: 'restart' }
    case 'invalid_mfa_code':
      return { message: 'That code is not valid.', effect: 'stay' }
    case 'invalid_enrolment_token':
      return {
        message: 'This enrolment link is invalid or expired, or the details do not match.',
        effect: 'stay',
      }
    case 'mfa_enrolment_expired':
      return { message: 'Setup took too long. Start again.', effect: 'restart_enrolment' }
    default:
      return { message: GENERIC_FAILURE, effect: 'stay' }
  }
}

/** Strips spaces/dashes and checks for exactly six digits. Returns null when malformed (never sent to the server). */
export function normaliseTotp(input: string): string | null {
  const digits = input.replace(/[\s-]/g, '')
  return /^\d{6}$/.test(digits) ? digits : null
}

/** Uppercases and checks the XXXXX-XXXXX shape (Crockford base32, ten characters). Null when malformed. */
export function normaliseRecoveryCode(input: string): string | null {
  const compact = input.replace(/[\s-]/g, '').toUpperCase()
  if (!/^[0-9A-Z]{10}$/.test(compact)) return null
  return `${compact.slice(0, 5)}-${compact.slice(5)}`
}

/** Minimum password length for an invitation (OpenAPI InvitationAcceptRequest.minLength; the server enforces it too). */
export const MIN_INVITATION_PASSWORD_LENGTH = 12

export const INVITATION_DEAD =
  'This invitation link is invalid, has expired or was already used, or the person who issued it is no longer active. Ask your administrator to send a new invitation.'

/** Failure of POST /v1/staff/invitations/accept. `dead` = the token is spent or unusable: drop it, nothing to retry. */
export function describeInvitationAcceptError(err: unknown): { message: string; dead: boolean } {
  if (!(err instanceof ApiError)) return { message: GENERIC_FAILURE, dead: false }
  switch (err.code) {
    case 'invalid_enrolment_token':
      return { message: INVITATION_DEAD, dead: true }
    case 'validation_error':
      return { message: `Choose a password of at least ${MIN_INVITATION_PASSWORD_LENGTH} characters.`, dead: false }
    case 'rate_limited':
      return {
        message: err.retryAfterSeconds ? `Too many attempts. Wait ${err.retryAfterSeconds} seconds and try again.` : 'Too many attempts. Wait a minute and try again.',
        dead: false,
      }
    case 'network_error':
      return { message: 'Could not reach the server. Check your connection and try again.', dead: false }
    case 'service_unavailable':
      return { message: 'Accepting invitations is temporarily unavailable. Try again shortly.', dead: false }
    default:
      return { message: 'The invitation could not be accepted. Try again.', dead: false }
  }
}
