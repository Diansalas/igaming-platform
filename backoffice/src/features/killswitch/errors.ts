// Closed code map for every non-2xx on the kill-switch routes. The server `message` is NEVER displayed
// (ADR 0114 invariant 7.9). Only the HTTP status, the closed `code` (if it matches a safe token) and the
// request id are used. A refusal is always shown, never swallowed.
import { ApiError } from '../../api/types'

export type KillSwitchOp = 'list' | 'engage' | 'request' | 'get_request' | 'approve' | 'cancel'

export interface KillSwitchErrorView {
  message: string
  requestId?: string
  status?: number
  code?: string
  /** True when what the user saw is stale and a refresh is the right next step; the dialogs refresh on it (refusalIsStale). */
  stale: boolean
}

const SAFE_CODE = /^[A-Za-z0-9_]{1,40}$/

/** The stable 409 code for a second filing while one is open (the one-open unique index). */
const ALREADY_OPEN = 'KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN'

/** True when the refusal means what the screen shows is out of date, so the caller refreshes. */
export function refusalIsStale(err: unknown, op: KillSwitchOp): boolean {
  return describeKillSwitchError(err, op).stale
}

const FORBIDDEN =
  'The server refused this: your account does not hold the permission for this action, or it may not act on this tenant. Nothing was changed.'

export function describeKillSwitchError(err: unknown, op: KillSwitchOp): KillSwitchErrorView {
  if (!(err instanceof ApiError)) {
    return { message: 'Something went wrong. Nothing was confirmed. Please try again.', stale: false }
  }
  const base = { requestId: err.requestId, status: err.status, code: SAFE_CODE.test(err.code) ? err.code : undefined, stale: false }
  if (err.code === 'network_error') return { ...base, message: 'Could not reach the server. Nothing was confirmed. Check your connection and try again.' }
  switch (err.status) {
    case 401:
      return { ...base, message: 'Your session has ended. Sign in again.' }
    case 403:
      return { ...base, message: FORBIDDEN }
    case 404:
      if (op === 'engage' || op === 'list') return { ...base, message: 'The server could not find this tenant. Nothing was changed.' }
      return { ...base, stale: true, message: 'This switch or release request no longer exists for this tenant. Nothing was changed. The list is being refreshed.' }
    case 409:
      if (op === 'engage')
        return { ...base, stale: true, message: 'The server refused because of a conflict, for example a switch engaged by platform staff can be changed only by platform staff. Nothing was changed. The list is being refreshed.' }
      if (op === 'request' && err.code === ALREADY_OPEN)
        return { ...base, stale: true, message: 'A release request is already open for this switch, so a second one was refused. Nothing was changed. The list is being refreshed; the open request will appear on this switch, where it can be approved by a different person or cancelled.' }
      if (op === 'request')
        return { ...base, stale: true, message: 'The server refused: the switch is not engaged any more, or it was engaged by platform staff and only platform staff may release it. Nothing was changed. The list is being refreshed.' }
      if (op === 'approve')
        return { ...base, stale: true, message: 'The server refused the approval. Common causes: you filed this request yourself (a different person must approve), the request expired or was already decided, the switch changed since the request was filed, or a platform lock applies. The switch was not released. The list is being refreshed.' }
      if (op === 'cancel')
        return { ...base, stale: true, message: 'The server refused the cancellation: the request is no longer open. Nothing was changed. The list is being refreshed.' }
      return { ...base, message: 'The server reported a conflict. Nothing was changed.' }
    case 400:
      if (op === 'engage')
        return { ...base, message: 'The server rejected the request as invalid. Check the provider (use * or a provider id registered on this deployment, with no spaces), the operation, and a reason code of 1 to 64 bytes. Nothing was changed.' }
      if (op === 'request')
        return { ...base, message: 'The server rejected the request as invalid. Check the reason code (1 to 64 bytes). Nothing was changed.' }
      return { ...base, message: 'The server rejected the request as invalid. Nothing was changed.' }
    case 429:
      return { ...base, message: 'Too many requests. Wait a moment and try again.' }
  }
  if (err.status >= 500) return { ...base, message: 'The server could not complete the request. Nothing was confirmed. Try again, and quote the request ID if it persists.' }
  return { ...base, message: 'The request could not be completed. Nothing was confirmed.' }
}
