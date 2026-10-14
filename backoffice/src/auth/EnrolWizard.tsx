// Authenticator enrolment (ADR 0119 s6.2, contract section 1.3). Used by the
// recovery-code flow (LoginPage) and the enrolment-link flow (EnrolPage).
//
// Secret handling: the TOTP secret, otpauth URI, recovery codes and the
// token pair exist ONLY in this component's React state. Nothing is written
// to localStorage/sessionStorage or logged. The session is not installed
// (so the user is not "signed in") until the recovery codes were shown and
// acknowledged; leaving the component discards everything.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import {
  staffEnrolConfirm,
  staffEnrolStart,
  type EnrolConfirmResponse,
  type EnrolStartResponse,
} from '../api/auth'
import { Button } from '../components/Button'
import { TextInput } from '../components/FormField'
import { useAuth } from './AuthContext'
import { formatRecoveryCodesFile, RecoveryCodesDisplay } from './RecoveryCodesDisplay'
import { describeStaffAuthError, GENERIC_FAILURE, normaliseTotp } from './staffAuthErrors'

interface Props {
  /** The `enrol` challenge token. */
  challengeToken: string
  /** The challenge is dead; the parent restarts the flow and shows `message`. */
  onRestart: (message: string) => void
  /** Optional lead-in, e.g. "Recovery code accepted." */
  intro?: string
}

/** Only a well-formed otpauth TOTP URI is ever used as a link target. */
export function isSafeOtpauthUri(uri: string): boolean {
  return /^otpauth:\/\/totp\/[^\s]+$/.test(uri)
}

export { formatRecoveryCodesFile }

export function EnrolWizard({ challengeToken, onRestart, intro }: Props) {
  const { establishSession } = useAuth()
  const [started, setStarted] = useState<EnrolStartResponse | null>(null)
  const [done, setDone] = useState<EnrolConfirmResponse | null>(null)
  const [code, setCode] = useState('')
  const [revealed, setRevealed] = useState(false)
  const [acked, setAcked] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const inFlight = useRef(false)
  const headingRef = useRef<HTMLHeadingElement>(null)

  // Best-effort: state is discarded with the component anyway; this only drops references early. JavaScript
  // cannot guarantee memory scrubbing.
  useEffect(
    () => () => {
      setStarted(null)
      setDone(null)
      setCode('')
    },
    [],
  )

  useEffect(() => {
    headingRef.current?.focus()
  }, [done, started])

  async function guarded(fn: () => Promise<void>) {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setError(null)
    try {
      await fn()
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  function fail(err: unknown) {
    const f = describeStaffAuthError(err, 'enrol')
    if (f.effect === 'restart') {
      setStarted(null)
      onRestart(f.message)
      return
    }
    if (f.effect === 'restart_enrolment') {
      setStarted(null)
      setCode('')
      setRevealed(false)
    }
    setError(f.message)
  }

  function start() {
    return guarded(async () => {
      try {
        setStarted(await staffEnrolStart(challengeToken))
        setCode('')
        setRevealed(false)
      } catch (err) {
        fail(err)
      }
    })
  }

  function confirm(e: FormEvent) {
    e.preventDefault()
    if (!started) return
    const digits = normaliseTotp(code)
    if (!digits) {
      setError('Enter the 6-digit code from your authenticator app.')
      return
    }
    return guarded(async () => {
      try {
        const res = await staffEnrolConfirm(challengeToken, started.factor_id, digits)
        setStarted(null)
        setCode('')
        setDone(res)
      } catch (err) {
        setCode('')
        fail(err)
      }
    })
  }

  async function copyUri() {
    if (!started) return
    try {
      await navigator.clipboard.writeText(started.otpauth_uri)
      setNotice('Setup URI copied to the clipboard. It contains your secret: clear your clipboard after adding the account.')
    } catch {
      setNotice('Copy failed. Reveal the secret and enter it manually.')
    }
  }

  function finish() {
    if (!done || !acked) return
    const pair = done.token_pair
    try {
      establishSession(pair)
    } catch {
      // Fail closed: a token that is not an aal2 session is never installed
      // (InsufficientAssuranceError); the flow restarts at the password step.
      onRestart(GENERIC_FAILURE)
    }
    setDone(null)
  }

  const errorEl = error && (
    <p id="enrol-error" role="alert" className="text-sm text-red-600">
      {error}
    </p>
  )
  const noticeEl = notice && (
    <p role="status" className="text-sm text-slate-600">
      {notice}
    </p>
  )

  if (done) {
    return (
      <section aria-labelledby="enrol-codes-heading" className="flex flex-col gap-4">
        <h2 id="enrol-codes-heading" ref={headingRef} tabIndex={-1} className="text-base font-semibold text-slate-900">
          Save your recovery codes
        </h2>
        <p className="text-sm text-slate-600">
          Each code can be used once if you lose your authenticator. They are shown only now and cannot be shown again.
        </p>
        <RecoveryCodesDisplay codes={done.recovery_codes} acked={acked} onAckChange={setAcked} />
        {errorEl}
        <Button type="button" disabled={!acked} onClick={finish}>
          Continue
        </Button>
      </section>
    )
  }

  if (!started) {
    return (
      <section aria-labelledby="enrol-heading" className="flex flex-col gap-4">
        <h2 id="enrol-heading" ref={headingRef} tabIndex={-1} className="text-base font-semibold text-slate-900">
          Set up your authenticator
        </h2>
        {intro && <p className="text-sm text-slate-600">{intro}</p>}
        <p className="text-sm text-slate-600">
          You need an authenticator app (any TOTP app) on your phone or computer.
        </p>
        {errorEl}
        <Button type="button" isLoading={busy} onClick={start}>
          Generate setup secret
        </Button>
      </section>
    )
  }

  return (
    <section aria-labelledby="enrol-scan-heading" className="flex flex-col gap-4">
      <h2 id="enrol-scan-heading" ref={headingRef} tabIndex={-1} className="text-base font-semibold text-slate-900">
        Add the account to your authenticator
      </h2>
      <p className="text-sm text-slate-600">
        Enter the setup key below in your authenticator app, or copy the setup URI into it. A QR code is not shown
        (no QR library is bundled yet).
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <Button type="button" variant="secondary" aria-pressed={revealed} onClick={() => setRevealed((v) => !v)}>
          {revealed ? 'Hide setup key' : 'Show setup key'}
        </Button>
        <Button type="button" variant="secondary" onClick={copyUri}>
          Copy setup URI
        </Button>
        {isSafeOtpauthUri(started.otpauth_uri) && (
          <a href={started.otpauth_uri} className="text-sm text-brand-700 underline">
            Open in authenticator app
          </a>
        )}
      </div>
      {revealed && (
        <div className="flex flex-col gap-1">
          <span className="text-sm font-medium text-slate-700">Setup key</span>
          <p className="select-all break-all rounded-md border border-border bg-surface-alt p-3 font-mono text-sm">
            {started.secret_base32}
          </p>
        </div>
      )}
      {noticeEl}
      <form onSubmit={confirm} className="flex flex-col gap-3" noValidate>
        <TextInput
          label="6-digit code"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          inputMode="numeric"
          id="enrol-code"
          aria-describedby={error ? 'enrol-code-help enrol-error' : 'enrol-code-help'}
          autoComplete="one-time-code"
          help="From your authenticator app, to confirm the setup."
        />
        {errorEl}
        <Button type="submit" isLoading={busy}>
          Confirm and continue
        </Button>
        <Button type="button" variant="ghost" onClick={start} disabled={busy}>
          Generate a new secret
        </Button>
      </form>
    </section>
  )
}
