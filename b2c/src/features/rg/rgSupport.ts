import { useRef } from 'react'
import { ApiError } from '../../api/types'
import { newIdempotencyKey } from '../../lib/idempotencyKey'

/**
 * Player-facing text for a failed responsible-gaming request. The server's closed RG_* codes are mapped to
 * a fixed sentence; anything else shows the server's own message (never an invented reason). A rate limit
 * and an unavailable service are retryable and say so.
 */
export function rgErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Please try again.'
  switch (err.code) {
    case 'RG_LIMIT_ABOVE_CEILING':
      return 'That amount is above the maximum allowed for this limit.'
    case 'RG_PERIOD_NOT_ALLOWED':
      return 'That period is not available for this limit.'
    case 'RG_DURATION_NOT_ALLOWED':
      return 'That duration is not available.'
    case 'RG_NOT_OFFERED':
      return 'This change is not available for your account.'
    case 'RG_NO_CHANGE':
      return 'That is already your current limit.'
    case 'RG_CHANGE_NOT_PENDING':
      return 'That change is no longer pending.'
    case 'RG_COOLING_OFF':
      return 'The cooling-off period for this increase has not finished yet.'
    case 'RG_CONFIG_MISSING':
      return 'Increasing or removing this limit is not available right now.'
    case 'RG_PENDING_CAP':
      return 'You have too many pending changes. Cancel one or wait for it to take effect.'
    case 'RG_IDENTITY_REVIEW':
      return 'Your account is under identity review, so this limit cannot be loosened yet.'
    case 'RG_IDEMPOTENCY_MISMATCH':
      return 'This request was already sent with different details. Please try again.'
    case 'rate_limited':
      return 'Too many changes in a short time. Please wait a moment and try again.'
    case 'RG_UNAVAILABLE':
    case 'service_unavailable':
      return 'This is temporarily unavailable. Please try again in a moment.'
    case 'network_error':
      return err.message
    default:
      return err.message || 'The request failed.'
  }
}

/**
 * One idempotency key per ATTEMPT: the same request body reuses the same key (a retry after an ambiguous
 * outcome can never create a second change), a different body gets a fresh key (the server answers 409 when a
 * key is reused with a different request), and `reset()` starts a new attempt after a success.
 */
export function useAttemptKey() {
  const ref = useRef<{ fingerprint: string; key: string } | null>(null)
  return {
    keyFor(fingerprint: string): string {
      if (!ref.current || ref.current.fingerprint !== fingerprint) {
        ref.current = { fingerprint, key: newIdempotencyKey() }
      }
      return ref.current.key
    },
    reset() {
      ref.current = null
    },
  }
}

export const PERIOD_LABELS: Record<string, string> = {
  rolling_24h: 'Rolling 24 hours',
  rolling_7d: 'Rolling 7 days',
  rolling_30d: 'Rolling 30 days',
  calendar_day: 'Calendar day',
  calendar_week: 'Calendar week',
  calendar_month: 'Calendar month',
}

export const KIND_LABELS: Record<string, string> = {
  deposit: 'Deposit limit',
  loss: 'Loss limit',
  wager: 'Wager limit',
}

/** Human text for a whole number of seconds (the server's cooling-off period). */
export function formatSeconds(seconds: number): string {
  if (seconds % 86400 === 0) {
    const d = seconds / 86400
    return `${d} day${d === 1 ? '' : 's'}`
  }
  if (seconds % 3600 === 0) {
    const h = seconds / 3600
    return `${h} hour${h === 1 ? '' : 's'}`
  }
  if (seconds % 60 === 0) {
    const m = seconds / 60
    return `${m} minute${m === 1 ? '' : 's'}`
  }
  return `${seconds} seconds`
}

export function formatHours(hours: number): string {
  if (hours % 24 === 0) {
    const d = hours / 24
    return `${d} day${d === 1 ? '' : 's'}`
  }
  return `${hours} hour${hours === 1 ? '' : 's'}`
}

export function formatWhen(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
