// Closed code map for every non-2xx on the AML routes. The server `message` is NEVER displayed: only the HTTP status, the
// closed `code`, the closed check code (if any) and the request id are used. A refusal is always shown, never swallowed.
// Nothing here names a person, a hold or a match: the AML surface is officer-only and says no more than the server did.
import { ApiError } from '../../api/types'

export type AmlOp =
  | 'queue'
  | 'case'
  | 'holds'
  | 'place_hold'
  | 'lift_hold'
  | 'open_case'
  | 'transition'
  | 'note'
  | 'flag_sar'
  | 'propose'
  | 'approve'

export interface AmlErrorView {
  message: string
  requestId?: string
  status?: number
  /** True when what the screen shows is out of date, so the caller refreshes. */
  stale: boolean
}

const NOTHING = 'Nothing was changed.'

const CHECK_TEXT: Record<string, { text: string; stale: boolean }> = {
  quorum_not_met: {
    text: 'The officer quorum for this tenant is not met: at least two distinct officers must hold an in-force AML officer grant before a new hold or screening policy can be activated.',
    stale: false,
  },
  hold_refused: {
    text: 'The server refused the hold operation. A hold from a sanctions list can be lifted only by a four-eyes proposal and approval on a case, not directly.',
    stale: true,
  },
  four_eyes_refused: {
    text: 'The server refused: the approver may not be the proposer, the same person as the proposer, or the subject of the case, and an officer may not act on a record about themselves.',
    stale: false,
  },
  stale: {
    text: 'The case changed since you opened it, or the proposal is no longer the latest event (any later event invalidates a pending proposal). The case is being refreshed.',
    stale: true,
  },
  illegal_transition: {
    text: 'That move is not allowed from the case’s current status. The case is being refreshed.',
    stale: true,
  },
  guard_refused: { text: 'A database guard refused the request.', stale: true },
  note_refused: {
    text: 'The note was refused by the server-side check: it must not contain card numbers, IBANs, e-mail addresses, phone numbers, document-like identifiers or control characters. It is a guard, not a guarantee: keep notes free of personal data.',
    stale: false,
  },
}

export function describeAmlError(err: unknown, op: AmlOp): AmlErrorView {
  if (!(err instanceof ApiError)) return { message: `Something went wrong. Nothing was confirmed. Please try again.`, stale: false }
  const base = { requestId: err.requestId, status: err.status, stale: false }
  if (err.code === 'network_error') return { ...base, message: 'Could not reach the server. Nothing was confirmed. Check your connection and try again.' }
  if (err.code === 'step_up_cancelled') return { ...base, message: `The extra verification was cancelled. ${NOTHING}` }
  if (err.code === 'step_up_failed') return { ...base, message: `The extra verification did not take effect. ${NOTHING}` }
  const check = err.checkCodes.find((c) => c in CHECK_TEXT)
  if (check) return { ...base, stale: CHECK_TEXT[check].stale, message: `${CHECK_TEXT[check].text} ${NOTHING}` }
  switch (err.status) {
    case 401:
      return { ...base, message: 'Your session has ended. Sign in again.' }
    case 403:
      return {
        ...base,
        message: `The server refused this: your account does not hold an in-force AML officer grant for this tenant, or the signed proof was refused. ${NOTHING}`,
      }
    case 404:
      return {
        ...base,
        stale: op !== 'queue' && op !== 'case' && op !== 'holds',
        message: `Not found: the record does not exist, is outside your AML scope, or belongs to another tenant. ${NOTHING}`,
      }
    case 400:
      return { ...base, message: `The server rejected the request as invalid. Check the identifiers and the reason code (lower-case letters, digits and underscores, up to 64). ${NOTHING}` }
    case 409:
      return { ...base, stale: true, message: `The server reported a conflict. ${NOTHING} The view is being refreshed.` }
    case 429:
      return { ...base, message: 'Too many requests. Wait a moment and try again.' }
    case 503:
      return { ...base, message: `AML officer functions are not available right now. Nothing was confirmed.` }
  }
  if (err.status >= 500) return { ...base, message: 'The server could not complete the request. Nothing was confirmed. Quote the request ID if it persists.' }
  return { ...base, message: 'The request could not be completed. Nothing was confirmed.' }
}
