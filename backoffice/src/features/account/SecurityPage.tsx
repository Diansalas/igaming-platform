import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { getMyMfaStatus, regenerateRecoveryCodes } from '../../api/auth'
import { ApiError } from '../../api/types'
import { RecoveryCodesDisplay } from '../../auth/RecoveryCodesDisplay'
import { UNAVAILABLE } from '../../auth/staffAuthErrors'
import { STEP_UP_MESSAGES } from '../../auth/stepUpCoordinator'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'

/** Warn at this many remaining recovery codes or fewer (ADR 0119 s15.4, contract 1.5). */
export const RECOVERY_WARN_AT = 3

function formatWhen(iso: string | null): string {
  if (!iso) return 'Not available'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? 'Not available' : d.toISOString().replace('T', ' ').slice(0, 16) + ' UTC'
}

/** Fixed strings keyed on the error code only; the server message is never rendered (ADR 0119 section 15). */
export function describeRegenerateError(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Recovery codes could not be regenerated. Try again.'
  switch (err.code) {
    case 'step_up_cancelled':
      return 'Cancelled. Your existing recovery codes are unchanged.'
    case 'step_up_failed':
      return 'Your identity could not be confirmed. Your existing recovery codes are unchanged.'
    case 'step_up_required':
      return STEP_UP_MESSAGES.not_effective
    case 'session_revoked':
    case 'unauthorized':
      return STEP_UP_MESSAGES.session_ended
    case 'forbidden':
      return 'You are not allowed to regenerate recovery codes.'
    case 'service_unavailable':
      return 'Recovery codes are temporarily unavailable. Try again.'
    case 'network_error':
      return 'Could not reach the server. Check the number of recovery codes remaining before trying again.'
    case 'rate_limited':
      return 'Too many attempts. Wait a minute and try again.'
    default:
      return 'Recovery codes could not be regenerated. Try again.'
  }
}

/**
 * "My security" (ADR 0119 S7a/S7b): MFA status from GET /v1/staff/me/mfa and recovery-code regeneration
 * (POST /v1/staff/mfa/recovery-codes/regenerate, step-up class critical). Display only - the server decides what
 * any staff member may do, including whether a step-up is needed: the step-up modal opens from the fetch layer when
 * the server answers 403 step_up_required, and the request is re-sent once.
 *
 * The new codes live ONLY in this component's state (not a mutation/query cache), there are no timers, and they are
 * dropped when the user acknowledges them or leaves the page.
 */
export function SecurityPage() {
  const q = useQuery({ queryKey: ['staff-mfa-status'], queryFn: getMyMfaStatus, retry: false })
  const queryClient = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [codes, setCodes] = useState<string[] | null>(null)
  const [acked, setAcked] = useState(false)
  const [regenError, setRegenError] = useState<string | null>(null)
  const inFlight = useRef(false)
  // Aborted on unmount: leaving the page must never let the interceptor re-send after a late step-up, because the
  // server would revoke the old codes and the new ones would have nowhere to be shown.
  const abortRef = useRef<AbortController | null>(null)
  const codesHeadingRef = useRef<HTMLHeadingElement>(null)

  useEffect(() => {
    if (codes) codesHeadingRef.current?.focus()
  }, [codes])

  // While the request is in flight a reload would lose the codes the server is about to issue.
  useEffect(() => {
    if (!busy) return
    const h = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', h)
    return () => window.removeEventListener('beforeunload', h)
  }, [busy])

  // Best-effort: drop the references when leaving the page (JavaScript cannot guarantee scrubbing).
  useEffect(
    () => () => {
      abortRef.current?.abort()
      setCodes(null)
    },
    [],
  )

  async function regenerate() {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setRegenError(null)
    setConfirming(false)
    const ctl = new AbortController()
    abortRef.current = ctl
    try {
      const res = await regenerateRecoveryCodes(ctl.signal)
      if (ctl.signal.aborted) return
      setAcked(false)
      setCodes(res.recovery_codes)
    } catch (err) {
      if (!ctl.signal.aborted) setRegenError(describeRegenerateError(err))
    } finally {
      inFlight.current = false
      if (ctl.signal.aborted) return
      setBusy(false)
      // The remaining count changed on success and is unknown after an indeterminate failure.
      void queryClient.invalidateQueries({ queryKey: ['staff-mfa-status'] })
    }
  }

  function finishCodes() {
    if (!acked) return
    setCodes(null)
    setAcked(false)
  }

  return (
    <div>
      <PageHeader title="My security" description="Your authenticator and recovery codes." />
      {q.isPending && <LoadingSpinner label="Loading your security status..." />}
      {q.isError && (
        <p role="alert" className="text-sm text-red-600">
          {q.error instanceof ApiError && q.error.code === 'service_unavailable'
            ? UNAVAILABLE
            : 'Could not load your security status. Try again.'}
        </p>
      )}
      {q.data && (
        <div className="flex max-w-xl flex-col gap-4">
          <Card title="Authenticator">
            <dl className="grid grid-cols-[10rem_1fr] gap-y-2 text-sm">
              <dt className="text-slate-500">Status</dt>
              <dd>
                <Badge tone={q.data.enrolled ? 'success' : 'danger'}>{q.data.enrolled ? 'Enrolled' : 'Not enrolled'}</Badge>
              </dd>
              <dt className="text-slate-500">Type</dt>
              <dd>{q.data.factor_type === 'totp' ? 'Authenticator app (TOTP)' : 'None'}</dd>
              <dt className="text-slate-500">Activated</dt>
              <dd>{formatWhen(q.data.activated_at)}</dd>
              <dt className="text-slate-500">Lock</dt>
              <dd>
                {q.data.hard_locked
                  ? 'Locked. Ask your administrator to reset your authenticator.'
                  : q.data.locked_until
                    ? `Temporarily locked until ${formatWhen(q.data.locked_until)}`
                    : 'Not locked'}
              </dd>
            </dl>
          </Card>
          <Card title="Recovery codes">
            <p className="text-sm">
              <span className="font-medium">{q.data.recovery_codes_remaining}</span> unused recovery codes remaining.
            </p>
            {q.data.enrolled && q.data.recovery_codes_remaining <= RECOVERY_WARN_AT && (
              <p role="alert" className="mt-2 text-sm text-amber-700">
                {q.data.recovery_codes_remaining === 0
                  ? 'You have no recovery codes left. Regenerate them below.'
                  : 'You are running low on recovery codes. Regenerate them below.'}
              </p>
            )}
            {codes ? (
              <section aria-labelledby="regen-codes-heading" className="mt-3 flex flex-col gap-3">
                <h3 id="regen-codes-heading" ref={codesHeadingRef} tabIndex={-1} className="text-sm font-semibold text-slate-900">
                  Your new recovery codes
                </h3>
                <p className="text-sm text-slate-600">
                  Your previous recovery codes no longer work. Each new code can be used once if you lose your
                  authenticator. They are shown only now and cannot be shown again.
                </p>
                <RecoveryCodesDisplay codes={codes} acked={acked} onAckChange={setAcked} />
                <div>
                  <Button type="button" disabled={!acked} onClick={finishCodes}>
                    Done
                  </Button>
                </div>
              </section>
            ) : (
              <div className="mt-3 flex flex-col gap-3">
                {regenError && (
                  <p role="alert" className="text-sm text-red-600">
                    {regenError}
                  </p>
                )}
                {confirming ? (
                  <div
                    role="group"
                    aria-label="Confirm regeneration"
                    className="flex flex-col gap-2 rounded-md border border-border bg-surface-alt p-3"
                  >
                    <p className="text-sm text-slate-700">
                      This replaces all of your unused recovery codes; the old ones stop working immediately. You may be
                      asked for a fresh code from your authenticator app.
                    </p>
                    <div className="flex gap-2">
                      <Button type="button" onClick={regenerate}>
                        Generate new codes
                      </Button>
                      <Button type="button" variant="secondary" onClick={() => setConfirming(false)}>
                        Cancel
                      </Button>
                    </div>
                  </div>
                ) : (
                  q.data.enrolled && (
                    <div>
                      <Button type="button" variant="secondary" isLoading={busy} onClick={() => setConfirming(true)}>
                        Regenerate recovery codes
                      </Button>
                    </div>
                  )
                )}
              </div>
            )}
          </Card>
        </div>
      )}
    </div>
  )
}
