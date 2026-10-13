// Staff sign-in, two steps (ADR 0119 s6.1, contract section 1): password ->
// `mfa` challenge -> TOTP (or recovery code -> forced re-enrolment). The UI
// shows a signed-in state only after the server returns an aal2 token pair
// (tokenStore refuses anything else). Challenge tokens live in React state
// only and are dropped on restart/unmount; the password is dropped as soon as
// the challenge is issued.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { staffLogin, staffMfaRecover, staffMfaVerify, type StaffAuthChallenge } from '../api/auth'
import { Button } from '../components/Button'
import { TextInput } from '../components/FormField'
import { useAuth } from './AuthContext'
import { EnrolWizard } from './EnrolWizard'
import {
  CHALLENGE_EXPIRED,
  describeStaffAuthError,
  GENERIC_FAILURE,
  normaliseRecoveryCode,
  normaliseTotp,
} from './staffAuthErrors'

type Phase = 'credentials' | 'mfa' | 'recovery' | 'enrol'

export function LoginPage() {
  const { establishSession, isAuthenticated } = useAuth()
  const location = useLocation()
  const [phase, setPhase] = useState<Phase>('credentials')
  const [challenge, setChallenge] = useState<StaffAuthChallenge | null>(null)
  const [tenantSlug, setTenantSlug] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inFlight = useRef(false)
  // Bumped on every restart so a late response for a cancelled attempt is dropped.
  const generation = useRef(0)
  // One absolute deadline per challenge, fixed when it is issued (not restarted on step switches).
  const [deadline, setDeadline] = useState<number | null>(null)

  const challengeToken = challenge?.token ?? null

  function restart(message: string | null) {
    generation.current++
    setDeadline(null)
    setChallenge(null)
    setPassword('')
    setCode('')
    setPhase('credentials')
    setError(message)
  }

  // The `mfa` challenge lives `expires_in` seconds; restart cleanly when it lapses. The timer does NOT run in
  // the 'enrol' phase: the server enforces that expiry, and a client restart there would wipe the recovery
  // codes or the session the user is about to receive.
  useEffect(() => {
    if (deadline === null || (phase !== 'mfa' && phase !== 'recovery')) return
    const t = setTimeout(() => restart(CHALLENGE_EXPIRED), Math.max(0, deadline - Date.now()))
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deadline, phase])

  // Drop all secrets on unmount.
  useEffect(
    () => () => {
      setChallenge(null)
      setPassword('')
      setCode('')
    },
    [],
  )

  if (isAuthenticated) {
    const from = (location.state as { from?: Location })?.from
    return <Navigate to={from?.pathname ?? '/'} replace />
  }

  async function run(step: 'login' | 'mfa', fn: (stale: () => boolean) => Promise<void>) {
    if (inFlight.current) return
    const gen = generation.current
    const stale = () => generation.current !== gen
    inFlight.current = true
    setBusy(true)
    setError(null)
    try {
      await fn(stale)
    } catch (err) {
      if (stale()) return
      const f = describeStaffAuthError(err, step)
      setCode('')
      if (f.effect === 'restart') restart(f.message)
      else setError(f.message)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  function onCredentials(e: FormEvent) {
    e.preventDefault()
    if (!email.trim() || !password) {
      setError('Enter your email and password.')
      return
    }
    return run('login', async (stale) => {
      const c = await staffLogin({ tenantSlug: tenantSlug.trim() || undefined, email: email.trim(), password })
      if (stale()) return
      if (c.type !== 'mfa' || !c.token) throw new Error('unexpected challenge')
      setPassword('')
      setChallenge(c)
      setDeadline(Date.now() + c.expires_in * 1000)
      setPhase('mfa')
    })
  }

  function onVerify(e: FormEvent) {
    e.preventDefault()
    if (!challengeToken) return
    const digits = normaliseTotp(code)
    if (!digits) {
      setError('Enter the 6-digit code from your authenticator app.')
      return
    }
    return run('mfa', async (stale) => {
      const pair = await staffMfaVerify(challengeToken, digits)
      if (stale()) return
      try {
        establishSession(pair)
      } catch {
        // Fail closed: not an aal2 session, so no signed-in state.
        restart(GENERIC_FAILURE)
      }
    })
  }

  function onRecover(e: FormEvent) {
    e.preventDefault()
    if (!challengeToken) return
    const rc = normaliseRecoveryCode(code)
    if (!rc) {
      setError('Enter a recovery code in the form XXXXX-XXXXX.')
      return
    }
    return run('mfa', async (stale) => {
      const c = await staffMfaRecover(challengeToken, rc)
      if (stale()) return
      if (c.type !== 'enrol' || !c.token) throw new Error('unexpected challenge')
      setCode('')
      setChallenge(c)
      setDeadline(null)
      setPhase('enrol')
    })
  }

  const errorEl = error && (
    <p id="auth-error" role="alert" className="text-sm text-red-600">
      {error}
    </p>
  )

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-alt px-4">
      <div className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-sm">
        <h1 className="mb-1 text-lg font-semibold text-slate-900">Operator Back Office</h1>

        {phase === 'credentials' && (
          <>
            <p className="mb-6 text-sm text-slate-500">Sign in with your staff credentials.</p>
            <form onSubmit={onCredentials} className="flex flex-col gap-4" noValidate>
              <TextInput
                label="Tenant slug"
                value={tenantSlug}
                onChange={(e) => setTenantSlug(e.target.value)}
                placeholder="Leave blank for platform admin"
                autoComplete="organization"
              />
              <TextInput
                label="Email"
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                autoComplete="username"
                aria-describedby={error ? 'auth-error' : undefined}
              />
              <TextInput
                label="Password"
                type="password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
                aria-describedby={error ? 'auth-error' : undefined}
              />
              {errorEl}
              <Button type="submit" isLoading={busy} className="w-full">
                Sign in
              </Button>
            </form>
          </>
        )}

        {phase === 'mfa' && (
          <>
            <p className="mb-6 text-sm text-slate-500">Enter the 6-digit code from your authenticator app.</p>
            <form onSubmit={onVerify} className="flex flex-col gap-4" noValidate>
              <TextInput
                label="Authentication code"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                inputMode="numeric"
                autoComplete="one-time-code"
                autoFocus
                aria-describedby={error ? 'auth-error' : undefined}
              />
              {errorEl}
              <Button type="submit" isLoading={busy} className="w-full">
                Verify
              </Button>
              <Button
                type="button"
                variant="ghost"
                onClick={() => {
                  setCode('')
                  setError(null)
                  setPhase('recovery')
                }}
              >
                Use a recovery code
              </Button>
              <Button type="button" variant="ghost" onClick={() => restart(null)}>
                Cancel
              </Button>
            </form>
          </>
        )}

        {phase === 'recovery' && (
          <>
            <p className="mb-6 text-sm text-slate-500">
              Enter one of your recovery codes. It can be used once, and you will then set up a new authenticator.
            </p>
            <form onSubmit={onRecover} className="flex flex-col gap-4" noValidate>
              <TextInput
                label="Recovery code"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                autoComplete="off"
                autoCapitalize="characters"
                spellCheck={false}
                autoFocus
                aria-describedby={error ? 'auth-error' : undefined}
              />
              {errorEl}
              <Button type="submit" isLoading={busy} className="w-full">
                Use recovery code
              </Button>
              <Button
                type="button"
                variant="ghost"
                onClick={() => {
                  setCode('')
                  setError(null)
                  setPhase('mfa')
                }}
              >
                Back to authenticator code
              </Button>
            </form>
          </>
        )}

        {phase === 'enrol' && challengeToken && (
          <EnrolWizard
            challengeToken={challengeToken}
            onRestart={(m) => restart(m)}
            intro="Recovery code accepted. Set up a new authenticator to finish signing in."
          />
        )}
      </div>
    </div>
  )
}
