// Closed code map for every non-2xx on the alert routes. The server `message` is NEVER displayed (ADR 0114
// invariant 7.9, ADR 0112 section 9). Only the closed `code`, the HTTP status and the request id are used.
import { ApiError } from '../../api/types'

export type AlertOp = 'list' | 'ack' | 'resolve' | 'status' | 'routes'

export interface AlertErrorView {
  message: string
  requestId?: string
  status?: number
  code?: string
  /** True when the state the user saw is stale and a refresh is the right next step. */
  stale: boolean
}

const FORBIDDEN = 'The server refused this: your account does not hold the alert permission, or this session is not platform scoped. Nothing was changed.'

const SAFE_CODE = /^[A-Za-z0-9_]{1,40}$/

export function describeAlertError(err: unknown, op: AlertOp): AlertErrorView {
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
      return { ...base, stale: true, message: 'This alert no longer exists on the server. Refresh the list.' }
    case 409:
      if (op === 'ack') return { ...base, stale: true, message: 'This alert is no longer open (it was already acknowledged or resolved), so it cannot be acknowledged. Nothing was changed. The list is being refreshed.' }
      if (op === 'resolve') return { ...base, stale: true, message: 'This alert is already resolved. Nothing was changed. The list is being refreshed.' }
      return { ...base, message: 'The server reported a conflict. Nothing was changed.' }
    case 400:
      if (op === 'ack' || op === 'resolve') return { ...base, message: 'The server rejected the request as invalid. Check the reason code (lowercase token, up to 64 characters) and try again.' }
      return { ...base, message: 'The server rejected the filters as invalid. Reset the filters and try again.' }
    case 429:
      return { ...base, message: 'Too many requests. Wait a moment and try again.' }
  }
  if (err.status >= 500) return { ...base, message: 'The server could not complete the request. Nothing was confirmed. Try again, and quote the request ID if it persists.' }
  return { ...base, message: 'The request could not be completed. Nothing was confirmed.' }
}
