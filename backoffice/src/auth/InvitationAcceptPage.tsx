// Staff invitation acceptance (ADR 0119 section 6.3, contract section 5): the invitee follows the emailed link
// (`/#/enrol?token=...&invite=1` or `/#/accept-invitation?token=...`), chooses a password, and continues into the
// authenticator enrolment wizard.
//
// Secret handling:
//  - The invitation token was captured from the URL FRAGMENT and stripped from the address bar before the router
//    rendered (enrolLink.ts). It lives in module memory only, is dropped as soon as it is spent or proven dead, and is
//    never written to storage, logs, the console or an error message.
//  - The password and its confirmation live only in this component's state, are cleared after every submit and are
//    never logged. The page renders no server-provided text: failures map from the error CODE to fixed wording.
//  - No existing account is revealed: every token failure (unknown, expired, used, revoked, issuer no longer active,
//    account not invited) is one server answer and one UI message.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Link, Navigate } from 'react-router-dom'
import { acceptStaffInvitation, type StaffAuthChallenge } from '../api/auth'
import { Button } from '../components/Button'
import { TextInput } from '../components/FormField'
import { useAuth } from './AuthContext'
import { clearInviteLinkToken, getInviteLinkToken } from './enrolLink'
import { EnrolWizard } from './EnrolWizard'
import { describeInvitationAcceptError, INVITATION_DEAD, MIN_INVITATION_PASSWORD_LENGTH } from './staffAuthErrors'

export const SETUP_ENDED =
  'Your setup session ended before the authenticator was confirmed. Your password is already set; ask your administrator to send an authenticator setup link.'

export function InvitationAcceptPage() {
  const { isAuthenticated } = useAuth()
  const [token, setToken] = useState<string | null>(() => getInviteLinkToken())
  const [challenge, setChallenge] = useState<StaffAuthChallenge | null>(null)
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [touched, setTouched] = useState(false)
  const inFlight = useRef(false)

  // A failed accept does not spend the token (only success does), so it stays for a retry while the page is open; it is
  // dropped from module memory when the page goes away.
  useEffect(() => () => clearInviteLinkToken(), [])

  if (isAuthenticated) {
    // A signed-in session must not be able to act on someone's invitation: drop the token and go home.
    clearInviteLinkToken()
    return <Navigate to="/" replace />
  }

  const tooShort = password.length < MIN_INVITATION_PASSWORD_LENGTH
  const mismatch = password !== confirm
  const showLength = touched && tooShort
  const showMismatch = touched && !tooShort && mismatch

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (!token || inFlight.current) return
    setTouched(true)
    if (tooShort || mismatch) return
    inFlight.current = true
    setBusy(true)
    setError(null)
    try {
      const c = await acceptStaffInvitation(token, password)
      if (c.type !== 'enrol' || !c.token) throw new Error('unexpected challenge')
      // The token is spent; drop it and the password.
      clearInviteLinkToken()
      setToken(null)
      setPassword('')
      setConfirm('')
      setChallenge(c)
    } catch (err) {
      const f = describeInvitationAcceptError(err)
      setPassword('')
      setConfirm('')
      setTouched(false)
      if (f.dead) {
        clearInviteLinkToken()
        setToken(null)
      }
      setError(f.message)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  function restart(message: string) {
    setChallenge(null)
    setError(message || SETUP_ENDED)
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-alt px-4">
      <main className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-sm">
        <h1 className="mb-1 text-lg font-semibold text-slate-900">Operator Back Office</h1>

        {challenge ? (
          <EnrolWizard challengeToken={challenge.token} onRestart={restart} intro="Password set. Now set up your authenticator." />
        ) : token ? (
          <>
            <h2 className="mb-1 text-base font-medium text-slate-800">Accept your invitation</h2>
            <p className="mb-6 text-sm text-slate-500">
              Choose a password of at least {MIN_INVITATION_PASSWORD_LENGTH} characters. Next you will set up an authenticator app.
            </p>
            <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate aria-label="Accept invitation">
              <TextInput
                label="New password"
                type="password"
                required
                autoComplete="new-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                aria-invalid={showLength || undefined}
                help={
                  showLength ? (
                    <span className="text-red-600">Password must be at least {MIN_INVITATION_PASSWORD_LENGTH} characters.</span>
                  ) : (
                    `At least ${MIN_INVITATION_PASSWORD_LENGTH} characters.`
                  )
                }
              />
              <TextInput
                label="Confirm new password"
                type="password"
                required
                autoComplete="new-password"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                aria-invalid={showMismatch || undefined}
                help={showMismatch ? <span className="text-red-600">The two passwords do not match.</span> : undefined}
              />
              {error && (
                <p id="accept-error" role="alert" className="text-sm text-red-600">
                  {error}
                </p>
              )}
              <Button type="submit" isLoading={busy} className="w-full">
                Set password and continue
              </Button>
            </form>
          </>
        ) : (
          <div className="flex flex-col gap-3">
            <p role="alert" className="text-sm text-red-600">
              {error ?? INVITATION_DEAD}
            </p>
            <Link to="/login" className="text-sm text-brand-700 underline">
              Go to sign in
            </Link>
          </div>
        )}
      </main>
    </div>
  )
}
