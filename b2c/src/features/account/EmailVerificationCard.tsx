import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { confirmEmailVerification, requestEmailVerification } from '../../api/auth'
import { ApiError } from '../../api/types'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { TextInput } from '../../components/TextInput'

function confirmErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case 'validation_error':
        return 'That code is invalid, expired or already used. Request a new code and try again.'
      case 'rate_limited':
        return 'Too many attempts. Wait a minute and try again.'
      case 'network_error':
        return err.message
      default:
        return err.message || 'Verification failed. Please try again.'
    }
  }
  return 'Verification failed. Please try again.'
}

function requestErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.code === 'unauthorized') return 'Your session has expired. Log in again to verify your email.'
    return err.message || 'Could not send a verification code. Please try again.'
  }
  return 'Could not send a verification code. Please try again.'
}

/**
 * Email verification for a pending_verification account, using only the
 * existing credential endpoints: request a code (sent by email), then
 * confirm it. On success the server moves the account to `active`; this
 * card only refetches GET /v1/me to show that - it never sets a status
 * itself.
 *
 * Staging: when the server runs its non-production account-activation test
 * support, the request endpoint returns the code in its response body. The
 * card then shows it, clearly labelled, and pre-fills the field - the
 * player still submits it through the normal confirm endpoint. With no
 * code in the response (production, or a rate-limited request), the player
 * types the code from their email.
 */
export function EmailVerificationCard() {
  const queryClient = useQueryClient()
  const [code, setCode] = useState('')
  const [requested, setRequested] = useState(false)
  const [stagingCode, setStagingCode] = useState<string | null>(null)

  const requestMutation = useMutation({
    mutationFn: requestEmailVerification,
    onSuccess: (body) => {
      setRequested(true)
      if (body.token) {
        setStagingCode(body.token)
        setCode(body.token)
      } else {
        setStagingCode(null)
      }
    },
  })

  const confirmMutation = useMutation({
    mutationFn: confirmEmailVerification,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['me'] })
    },
  })

  function onConfirm(e: FormEvent) {
    e.preventDefault()
    if (!code.trim() || confirmMutation.isPending) return
    confirmMutation.mutate(code.trim())
  }

  return (
    <Card title="Verify your email">
      <div className="flex flex-col gap-4 text-sm">
        <p className="text-slate-600">
          Your account is pending verification. Deposits, bets and casino play are available once your email is
          verified.
        </p>

        <div>
          <Button
            variant={requested ? 'secondary' : 'primary'}
            onClick={() => requestMutation.mutate()}
            isLoading={requestMutation.isPending}
          >
            {requested ? 'Resend verification code' : 'Send verification code'}
          </Button>
        </div>
        {requestMutation.error && (
          <p role="alert" className="text-red-700">
            {requestErrorMessage(requestMutation.error)}
          </p>
        )}
        {requested && !stagingCode && !requestMutation.isPending && (
          <p role="status" className="text-slate-600">
            If a code could be sent, it is on its way to your email. Enter it below.
          </p>
        )}
        {stagingCode && (
          <div role="status" className="rounded-md border border-amber-300 bg-amber-50 p-3 text-amber-900">
            <p className="font-medium">Staging only: the server returned your verification code.</p>
            <p className="mt-1 break-all font-mono text-xs">{stagingCode}</p>
            <p className="mt-1 text-xs">It has been filled in below. Production servers only send it by email.</p>
          </div>
        )}

        <form className="flex flex-col gap-2 sm:flex-row sm:items-end" onSubmit={onConfirm}>
          <label className="flex flex-1 flex-col gap-1">
            <span className="text-slate-600">Verification code</span>
            <TextInput value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" />
          </label>
          <Button type="submit" isLoading={confirmMutation.isPending} disabled={!code.trim()}>
            Verify email
          </Button>
        </form>
        {confirmMutation.error && (
          <p role="alert" className="text-red-700">
            {confirmErrorMessage(confirmMutation.error)}
          </p>
        )}
      </div>
    </Card>
  )
}
