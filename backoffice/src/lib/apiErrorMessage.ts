import { ApiError } from '../api/types'

export interface ErrorDetails {
  message: string
  requestId?: string
}

/**
 * Normalizes any thrown mutation error into the exact server message plus
 * its request id (for support correlation). The API's own message is
 * surfaced verbatim - the UI never rewrites or second-guesses why the
 * backend refused an action.
 */
export function describeError(err: unknown, fallback: string): ErrorDetails {
  if (err instanceof ApiError) return { message: err.message, requestId: err.requestId }
  if (err instanceof Error && err.message) return { message: err.message }
  return { message: fallback }
}

/** Single-line form for places that only accept a string (e.g. ConfirmDialog's errorMessage). */
export function formatError(details: ErrorDetails | null): string | null {
  if (!details) return null
  return details.requestId ? `${details.message} (Request ID: ${details.requestId})` : details.message
}
