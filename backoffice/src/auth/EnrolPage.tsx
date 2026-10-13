// Enrolment-link entry (`/#/enrol?token=...`, contract section 1.3 step 0).
// The one-time token was captured and stripped from the URL before the router
// rendered (enrolLink.ts). The staff member proves identity with their CURRENT
// password; the response is an `enrol` challenge handed to EnrolWizard.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Link, Navigate } from 'react-router-dom'
import { staffEnrolRedeem, type StaffAuthChallenge } from '../api/auth'
import { Button } from '../components/Button'
import { TextInput } from '../components/FormField'
import { useAuth } from './AuthContext'
import { clearEnrolLinkToken, getEnrolLinkToken } from './enrolLink'
import { EnrolWizard } from './EnrolWizard'
import { describeStaffAuthError } from './staffAuthErrors'

export function EnrolPage() {
  const { isAuthenticated } = useAuth()
  const [token, setToken] = useState<string | null>(() => getEnrolLinkToken())
  const [challenge, setChallenge] = useState<StaffAuthChallenge | null>(null)
  const [tenantSlug, setTenantSlug] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inFlight = useRef(false)

  // The token is not burned by a failed redeem (only success consumes it), so it stays for a retry while
  // the page is open; it is dropped from module memory when the page goes away.
  useEffect(() => () => clearEnrolLinkToken(), [])

  if (isAuthenticated) return <Navigate to="/" replace />

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (!token || inFlight.current) return
    if (!email.trim() || !password) {
      setError('Enter your email and your current password.')
      return
    }
    inFlight.current = true
    setBusy(true)
    setError(null)
    try {
      const c = await staffEnrolRedeem({
        token,
        tenantSlug: tenantSlug.trim() || undefined,
        email: email.trim(),
        password,
      })
      if (c.type !== 'enrol' || !c.token) throw new Error('unexpected challenge')
      // The one-time token is spent; drop it and the password.
      clearEnrolLinkToken()
      setToken(null)
      setPassword('')
      setChallenge(c)
    } catch (err) {
      setPassword('')
      setError(describeStaffAuthError(err, 'redeem').message)
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  function restart(message: string) {
    setChallenge(null)
    setError(message)
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-alt px-4">
      <div className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-sm">
        <h1 className="mb-1 text-lg font-semibold text-slate-900">Operator Back Office</h1>

        {challenge ? (
          <EnrolWizard challengeToken={challenge.token} onRestart={restart} />
        ) : token ? (
          <>
            <p className="mb-6 text-sm text-slate-500">
              Confirm your details to set up your authenticator. Use your current password.
            </p>
            <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
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
              />
              <TextInput
                label="Password"
                type="password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
                aria-describedby={error ? 'redeem-error' : undefined}
              />
              {error && (
                <p id="redeem-error" role="alert" className="text-sm text-red-600">
                  {error}
                </p>
              )}
              <Button type="submit" isLoading={busy} className="w-full">
                Continue
              </Button>
            </form>
          </>
        ) : (
          <div className="flex flex-col gap-3">
            <p role="alert" className="text-sm text-red-600">
              {error ?? 'This enrolment link is invalid or expired, or the details do not match.'}
            </p>
            <Link to="/login" className="text-sm text-brand-700 underline">
              Go to sign in
            </Link>
          </div>
        )}
      </div>
    </div>
  )
}
