// Step-up modal host (ADR 0119 section 15 item 3, slice S7b; contract
// docs/plans/adr0119-s4b-api-contract.md section 5).
//
// apiFetch (the fetch-layer interceptor) asks the coordinator for a step-up when the server answers
// `403 step_up_required`; this component renders the ONE shared modal, collects a fresh TOTP code, calls
// POST /v1/staff/auth/step-up, installs the returned access token and releases the waiting requests.
//
// Rules this file keeps:
//  - The code is never stored: the input is uncontrolled, its value is read once on submit and wiped from the DOM
//    immediately, it is never placed in React state, the coordinator, a query cache, Web Storage or a log.
//  - Fixed strings keyed on the error `code`; the server `message` is never rendered.
//  - The UI decides nothing about authorization or the step-up class; it only reacts to the server's answers.
import { useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore, type FormEvent, type ReactNode } from 'react'
import { staffStepUp } from '../api/auth'
import { ApiError } from '../api/types'
import { Button } from '../components/Button'
import { useAuth } from './AuthContext'
import {
  abortStepUp,
  completeStepUp,
  getStepUpPrompt,
  registerStepUpHost,
  STEP_UP_MESSAGES,
  subscribeStepUp,
  type StepUpAbortReason,
} from './stepUpCoordinator'
import { normaliseTotp } from './staffAuthErrors'
import { clearSession, getAccessToken, replaceAccessToken } from './tokenStore'

export const STEP_UP_INVALID_CODE = 'That code is not valid or was already used. Wait for the next code from your app and try again.'
export const STEP_UP_MALFORMED = 'Enter the 6-digit code from your authenticator app.'
export const STEP_UP_UNAVAILABLE = 'Verification is temporarily unavailable. Try again.'
export const STEP_UP_NETWORK = 'Could not reach the server. Check your connection and try again.'
export const STEP_UP_RENEWED = 'Your session was renewed. Enter a new code and try again.'
export const STEP_UP_GENERIC = 'Verification failed. Try again.'

type Failure = { kind: 'stay'; message: string } | { kind: 'end'; reason: StepUpAbortReason }

/** Maps a step-up endpoint failure to a fixed message or to the end of the prompt. Branches on `code` only. */
export function describeStepUpError(err: unknown): Failure {
  if (!(err instanceof ApiError)) return { kind: 'stay', message: STEP_UP_GENERIC }
  switch (err.code) {
    case 'invalid_mfa_code':
      return { kind: 'stay', message: STEP_UP_INVALID_CODE }
    case 'validation_error':
      return { kind: 'stay', message: STEP_UP_MALFORMED }
    case 'mfa_locked':
      return { kind: 'end', reason: 'locked' }
    case 'unauthorized':
      // The token was refreshed (or the session is gone, which the dialog detects): the code was NOT re-sent.
      return { kind: 'stay', message: STEP_UP_RENEWED }
    case 'session_revoked':
    case 'forbidden':
      return { kind: 'end', reason: 'session_ended' }
    case 'rate_limited':
      return {
        kind: 'stay',
        message: err.retryAfterSeconds
          ? `Too many attempts. Wait ${err.retryAfterSeconds} seconds and try again.`
          : 'Too many attempts. Wait a minute and try again.',
      }
    case 'service_unavailable':
      // One fixed message whatever the outcome would have been.
      return { kind: 'stay', message: STEP_UP_UNAVAILABLE }
    case 'network_error':
      return { kind: 'stay', message: STEP_UP_NETWORK }
    default:
      return { kind: 'stay', message: STEP_UP_GENERIC }
  }
}

function describeWindow(maxAge: number | undefined): string {
  if (!maxAge) return 'This action needs a recent verification.'
  const minutes = Math.max(1, Math.round(maxAge / 60))
  return `This action needs a verification from the last ${minutes} minute${minutes === 1 ? '' : 's'}.`
}

const FOCUSABLE = 'button:not([disabled]), input:not([disabled]), a[href], select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

function StepUpDialog({ maxAge, onEnd }: { maxAge: number | undefined; onEnd: (reason: StepUpAbortReason) => void }) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const inFlight = useRef(false)
  const closed = useRef(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const onEndRef = useRef(onEnd)
  onEndRef.current = onEnd

  // Focus the field, trap Tab, and own Escape. The listener is on window in the CAPTURE phase and stops
  // propagation so an underlying dialog's own Escape handler (a governed dialog the action came from) does not
  // also close.
  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null
    // Keep the element itself: React clears the ref before this cleanup runs, but the node (and its value) lives on.
    const inputEl = inputRef.current
    inputEl?.focus()
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        cancel()
        return
      }
      if (e.key !== 'Tab' || !dialogRef.current) return
      const items = Array.from(dialogRef.current.querySelectorAll<HTMLElement>(FOCUSABLE))
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      const active = document.activeElement
      if (!dialogRef.current.contains(active)) {
        e.preventDefault()
        first.focus()
      } else if (e.shiftKey && active === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && active === last) {
        e.preventDefault()
        first.focus()
      }
    }
    window.addEventListener('keydown', onKeyDown, true)
    return () => {
      window.removeEventListener('keydown', onKeyDown, true)
      // Never leave a code behind in the DOM.
      if (inputEl) inputEl.value = ''
      if (previous && document.contains(previous)) previous.focus()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function cancel() {
    if (closed.current) return
    closed.current = true
    if (inputRef.current) inputRef.current.value = ''
    onEndRef.current('cancelled')
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (inFlight.current || closed.current) return
    const raw = inputRef.current?.value ?? ''
    if (inputRef.current) inputRef.current.value = ''
    const digits = normaliseTotp(raw)
    if (!digits) {
      // A typo is not sent, so it does not spend one of the 5 attempts before the factor locks.
      setError(STEP_UP_MALFORMED)
      inputRef.current?.focus()
      return
    }
    inFlight.current = true
    setBusy(true)
    setError(null)
    try {
      const res = await staffStepUp(digits)
      // Even if the user cancelled while this was in flight the verified token is the session's newest: install it
      // (the original action still fails as cancelled, nothing is re-sent).
      replaceAccessToken(res.access_token)
      if (!closed.current) {
        closed.current = true
        completeStepUp()
      }
    } catch (err) {
      if (closed.current) return
      const f =
        (err instanceof Error && err.name === 'InsufficientAssuranceError') || getAccessToken() === null
          ? ({ kind: 'end', reason: 'session_ended' } as const)
          : describeStepUpError(err)
      if (f.kind === 'end') {
        if (f.reason === 'session_ended') clearSession()
        closed.current = true
        onEndRef.current(f.reason)
      } else {
        setError(f.message)
        inputRef.current?.focus()
      }
    } finally {
      inFlight.current = false
      if (!closed.current) setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-slate-900/50 p-4">
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="stepup-title"
        aria-describedby="stepup-desc"
        aria-busy={busy}
        className="w-full max-w-md rounded-lg bg-white p-4 shadow-xl"
      >
        <h2 id="stepup-title" className="text-sm font-semibold text-slate-900">
          Confirm it is you
        </h2>
        <p id="stepup-desc" className="mt-2 text-sm text-slate-600">
          {describeWindow(maxAge)} Enter the current 6-digit code from your authenticator app. Recovery codes are not accepted here.
        </p>
        <form onSubmit={submit} noValidate className="mt-3 flex flex-col gap-3">
          <div className="flex flex-col gap-1 text-sm">
            <label htmlFor="stepup-code" className="font-medium text-slate-700">
              6-digit code
            </label>
            <input
              id="stepup-code"
              ref={inputRef}
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              autoCapitalize="off"
              spellCheck={false}
              maxLength={12}
              aria-invalid={error ? true : undefined}
              aria-describedby={error ? 'stepup-error' : undefined}
              className="rounded-md border border-border bg-white px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
            />
          </div>
          {error && (
            <p id="stepup-error" role="alert" className="text-sm text-red-600">
              {error}
            </p>
          )}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="secondary" onClick={cancel}>
              Cancel
            </Button>
            <Button type="submit" isLoading={busy}>
              Verify
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}

export function StepUpProvider({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth()
  const prompt = useSyncExternalStore(subscribeStepUp, getStepUpPrompt, getStepUpPrompt)
  const [notice, setNotice] = useState<string | null>(null)
  const appRef = useRef<HTMLDivElement>(null)

  // While the modal is open the rest of the app is inert and hidden from assistive technology. A layout effect so
  // the attributes are removed BEFORE the dialog's cleanup restores focus to the control that started the action.
  const open = prompt !== null
  useLayoutEffect(() => {
    const el = appRef.current
    if (!open || !el) return
    el.setAttribute('inert', '')
    el.setAttribute('aria-hidden', 'true')
    return () => {
      el.removeAttribute('inert')
      el.removeAttribute('aria-hidden')
    }
  }, [open])

  useEffect(() => registerStepUpHost(), [])

  // The session ended while a prompt was open (another call's 401, sign-out): nothing may be re-sent.
  useEffect(() => {
    if (!isAuthenticated && getStepUpPrompt()) end('session_ended')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isAuthenticated])

  function end(reason: StepUpAbortReason) {
    abortStepUp(reason)
    setNotice(STEP_UP_MESSAGES[reason])
  }

  // A fresh prompt replaces any earlier outcome notice.
  useEffect(() => {
    if (prompt) setNotice(null)
  }, [prompt])

  return (
    <>
      <div ref={appRef} style={{ display: 'contents' }}>
        {children}
      </div>
      {prompt && <StepUpDialog maxAge={prompt.maxAge} onEnd={end} />}
      {notice && (
        <div role="status" className="fixed bottom-4 right-4 z-[70] flex max-w-sm items-start gap-3 rounded-md border border-border bg-white p-3 text-sm text-slate-800 shadow-lg">
          <span>{notice}</span>
          <button type="button" aria-label="Dismiss" onClick={() => setNotice(null)} className="text-slate-400 hover:text-slate-600">
            &#10005;
          </button>
        </div>
      )}
    </>
  )
}
