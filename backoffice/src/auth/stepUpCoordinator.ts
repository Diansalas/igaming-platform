// Single-flight coordinator for the step-up flow (ADR 0119 S4b/S7b, contract
// docs/plans/adr0119-s4b-api-contract.md section 5).
//
// Many requests can be refused with `403 step_up_required` at the same moment. They all wait on ONE shared
// prompt (one modal, one TOTP entry); when it succeeds every waiter is released to re-send its own request
// exactly once (apiFetch does the re-send), when it ends any other way every waiter is rejected with a
// fixed-message ApiError and nothing is re-sent.
//
// This module holds NO secret: the TOTP code lives only inside the dialog component's uncontrolled input and
// is never passed through here. It makes no authorization decision: whether a step-up is needed is the
// server's answer, whether the original action is then allowed is the server's answer again.
import { ApiError } from '../api/types'
import { getAccessToken, subscribeSession } from './tokenStore'

/** How a prompt ended without a fresh token. Maps to fixed, code-keyed messages (never server text). */
export type StepUpAbortReason = 'cancelled' | 'locked' | 'session_ended' | 'no_ui'

export const STEP_UP_MESSAGES = {
  cancelled: 'Verification cancelled. The action was not performed.',
  locked:
    'Too many incorrect codes. The action was not performed. Try again later or ask your administrator to reset your authenticator.',
  session_ended: 'Your session has ended. Sign in again.',
  no_ui: 'This action needs a fresh verification that cannot be requested here. The action was not performed.',
  not_effective: 'Verification did not take effect, so the action was not performed. Try again.',
} as const

export function stepUpAbortError(reason: StepUpAbortReason): ApiError {
  switch (reason) {
    case 'cancelled':
      return new ApiError('step_up_cancelled', STEP_UP_MESSAGES.cancelled, 403)
    case 'session_ended':
      return new ApiError('session_revoked', STEP_UP_MESSAGES.session_ended, 401)
    case 'locked':
      return new ApiError('step_up_failed', STEP_UP_MESSAGES.locked, 403)
    default:
      return new ApiError('step_up_failed', STEP_UP_MESSAGES.no_ui, 403)
  }
}

/** The second consecutive `step_up_required` for one request: never prompt again, never loop. */
export function stepUpNotEffectiveError(requestId?: string): ApiError {
  return new ApiError('step_up_required', STEP_UP_MESSAGES.not_effective, 403, requestId)
}

export interface StepUpPrompt {
  /** `max_age` seconds of the first refusal, used only to explain the prompt. */
  maxAge?: number
}

interface Pending {
  promise: Promise<void>
  resolve: () => void
  reject: (e: unknown) => void
  prompt: StepUpPrompt
}

let pending: Pending | null = null
// The access token minted by the last successful step-up. A request that was SENT before it (a different
// token) and is refused afterwards has already been covered by that step-up: it is released without a new
// prompt. Compared by identity against the current token, so any refresh invalidates it.
let steppedUpToken: string | null = null
let uiCount = 0
const listeners = new Set<() => void>()

// A cleared session forgets the stepped-up token (it must not outlive the session it belonged to).
subscribeSession(() => {
  if (getAccessToken() === null) steppedUpToken = null
})

function emit() {
  for (const l of listeners) l()
}

export function subscribeStepUp(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function getStepUpPrompt(): StepUpPrompt | null {
  return pending ? pending.prompt : null
}

/** The mounted dialog host registers itself; with none mounted a prompt could never be answered. */
export function registerStepUpHost(): () => void {
  uiCount++
  return () => {
    uiCount--
    if (uiCount === 0) abortStepUp('cancelled')
  }
}

/**
 * Called by apiFetch for a refused request. `sentToken` is the access token that request carried.
 * Resolves when the caller may re-send once; rejects with an ApiError when it must not.
 */
export function requestStepUp(sentToken: string | null, maxAge?: number): Promise<void> {
  const current = getAccessToken()
  if (current === null) return Promise.reject(stepUpAbortError('session_ended'))
  if (steppedUpToken !== null && current === steppedUpToken && sentToken !== current) return Promise.resolve()
  if (pending) return pending.promise
  if (uiCount === 0) return Promise.reject(stepUpAbortError('no_ui'))
  let resolve!: () => void
  let reject!: (e: unknown) => void
  const promise = new Promise<void>((res, rej) => {
    resolve = res
    reject = rej
  })
  // Rejections are delivered to every waiter; this guard only prevents an "unhandled" report for the shared promise.
  promise.catch(() => undefined)
  pending = { promise, resolve, reject, prompt: { maxAge } }
  emit()
  return promise
}

/** The dialog verified a code and the new access token is installed: release every waiter. */
export function completeStepUp(): void {
  if (!pending) return
  steppedUpToken = getAccessToken()
  const p = pending
  pending = null
  emit()
  p.resolve()
}

/** End the prompt without a fresh token: every waiter fails visibly and nothing is re-sent. */
export function abortStepUp(reason: StepUpAbortReason): void {
  if (!pending) return
  const p = pending
  pending = null
  emit()
  p.reject(stepUpAbortError(reason))
}

/** Test seam: forget everything (module state outlives a test file's components). */
export function resetStepUpForTests(): void {
  pending = null
  steppedUpToken = null
  uiCount = 0
}
