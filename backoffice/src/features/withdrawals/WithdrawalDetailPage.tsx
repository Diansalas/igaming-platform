import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  approveWithdrawal,
  getWithdrawal,
  rejectWithdrawal,
  resolveWithdrawal,
  submitWithdrawal,
  type SubmittedWithdrawal,
} from '../../api/withdrawals'
import { useAuth } from '../../auth/AuthContext'
import { InfoNote } from '../../components/Alert'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorState } from '../../components/ErrorState'
import { TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { ApiError } from '../../api/types'
import { describeError, formatError } from '../../lib/apiErrorMessage'
import { formatMoney } from '../../lib/money'
import { useOnceGuard } from '../../lib/useOnceGuard'
import { withdrawalStatusTone } from './status'

type DialogState = 'approve' | 'reject' | 'submit' | 'resolve' | null

export const SECOND_APPROVER_MESSAGE = 'Approval recorded — a second, different approver is still required.'

export const PAYMENT_METHOD_MISMATCH_MESSAGE =
  "The payment method does not match the rail of this withdrawal's payout instrument. Nothing was sent and the withdrawal is unchanged. The details shown have been reloaded; check the instrument rail and try again."
export const PAYOUT_DESTINATION_NOT_USABLE_MESSAGE =
  "The player's payout destination cannot be used right now. Nothing was sent and the withdrawal stays approved. It can be submitted again once the destination is usable; if it is not, escalate to compliance."

/** Friendly text for the two submit refusals introduced by the payout-instrument binding; other errors are shown verbatim. */
function submitErrorMessage(err: unknown): string | null {
  if (err instanceof ApiError) {
    const suffix = err.requestId ? ` (Request ID: ${err.requestId})` : ''
    if (err.code === 'PAYMENT_METHOD_MISMATCH') return PAYMENT_METHOD_MISMATCH_MESSAGE + suffix
    if (err.code === 'PAYOUT_DESTINATION_NOT_USABLE') return PAYOUT_DESTINATION_NOT_USABLE_MESSAGE + suffix
  }
  return null
}

function providerOutcomeMessage(verb: string, result: SubmittedWithdrawal): string {
  const provider = result.provider_id ? ` to provider ${result.provider_id}` : ''
  const reference = result.provider_reference ? ` (reference ${result.provider_reference})` : ''
  return `Withdrawal ${verb}${provider}${reference}. Current state: ${result.state}.`
}

/**
 * This page moves real money: every action here requires an explicit
 * confirmation dialog (never a bare button that fires the mutation), and
 * the four-eyes/distinct-approver/beneficiary/threshold rules are entirely
 * server-side (internal/withdrawal.Approve/Reject) - this page never
 * computes whether an approval "should" succeed, it only renders what the
 * server returns and surfaces exactly the error the server gives back if
 * it refuses (e.g. self-approval, duplicate decision, not-linked approver).
 *
 * Actions offered per state mirror the backend's own state checks:
 *   pending_review -> approve / reject   (409 "not awaiting review" otherwise)
 *   approved       -> submit             (409 "not approved and ready" otherwise)
 *   submitted      -> resolve            (409 "not submitted" otherwise)
 * A `requested` withdrawal is NOT decidable - it must first be promoted by
 * opening the Pending review queue (GET /v1/admin/withdrawals).
 */
export function WithdrawalDetailPage() {
  const { id = '' } = useParams()
  const queryClient = useQueryClient()
  const { claims } = useAuth()
  const [dialog, setDialog] = useState<DialogState>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [successMessage, setSuccessMessage] = useState<string | null>(null)
  const [paymentMethod, setPaymentMethod] = useState('')
  // This page moves real money via a distinct-approver/four-eyes-enforced
  // endpoint - a stray double-submit (see useOnceGuard's doc comment) is a
  // genuinely serious operational risk here, not just a UX nuisance, so
  // every action gets its own synchronous once-guard on top of the
  // dialogs' own isSubmitting-disables-the-button behavior.
  const approveGuard = useOnceGuard()
  const rejectGuard = useOnceGuard()
  const submitGuard = useOnceGuard()
  const resolveGuard = useOnceGuard()

  const query = useQuery({
    queryKey: ['withdrawal', id],
    queryFn: () => getWithdrawal(id),
    enabled: !!id,
  })

  async function invalidate() {
    await queryClient.invalidateQueries({ queryKey: ['withdrawal', id] })
    await queryClient.invalidateQueries({ queryKey: ['withdrawals'] })
  }

  function closeDialog() {
    setDialog(null)
    setActionError(null)
  }

  const approveMutation = useMutation({
    mutationFn: () => approveWithdrawal(id),
    onSuccess: async (result) => {
      setDialog(null)
      setActionError(null)
      setSuccessMessage(result.approved ? 'Withdrawal approved.' : SECOND_APPROVER_MESSAGE)
      await invalidate()
    },
    onError: (err) => setActionError(formatError(describeError(err, 'Failed to approve this withdrawal.'))),
    onSettled: () => approveGuard.release(),
  })

  const rejectMutation = useMutation({
    mutationFn: (reasonCode: string) => rejectWithdrawal(id, reasonCode),
    onSuccess: async () => {
      setDialog(null)
      setActionError(null)
      setSuccessMessage('Withdrawal rejected.')
      await invalidate()
    },
    onError: (err) => setActionError(formatError(describeError(err, 'Failed to reject this withdrawal.'))),
    onSettled: () => rejectGuard.release(),
  })

  const submitMutation = useMutation({
    mutationFn: (method: string) => submitWithdrawal(id, method),
    onSuccess: async (result) => {
      setDialog(null)
      setActionError(null)
      setSuccessMessage(providerOutcomeMessage('submitted', result))
      await invalidate()
    },
    onError: (err) => {
      setActionError(submitErrorMessage(err) ?? formatError(describeError(err, 'Failed to submit this withdrawal.')))
      // A mismatch means the page's view of the bound instrument is stale: reload it.
      if (err instanceof ApiError && err.code === 'PAYMENT_METHOD_MISMATCH') void invalidate()
    },
    onSettled: () => submitGuard.release(),
  })

  const resolveMutation = useMutation({
    mutationFn: () => resolveWithdrawal(id),
    onSuccess: async (result) => {
      setDialog(null)
      setActionError(null)
      setSuccessMessage(
        result.state === 'submitted'
          ? `The provider has not settled this withdrawal yet; it remains submitted. Try resolving again later.`
          : providerOutcomeMessage('resolved', result),
      )
      await invalidate()
    },
    onError: (err) => setActionError(formatError(describeError(err, 'Failed to resolve this withdrawal.'))),
    onSettled: () => resolveGuard.release(),
  })

  if (query.isLoading) return <LoadingSpinner label="Loading withdrawal..." />
  if (query.error) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  const withdrawal = query.data
  if (!withdrawal) return null

  const canDecide = withdrawal.state === 'pending_review'
  const canSubmit = withdrawal.state === 'approved'
  const canResolve = withdrawal.state === 'submitted'
  const boundInstrument = withdrawal.payout_instrument ?? null
  // Bound: the rail is the instrument's and is not editable. Legacy (no binding): the staff input, no default.
  const effectiveMethod = boundInstrument ? boundInstrument.rail : paymentMethod.trim()
  const amountLabel = formatMoney(withdrawal.amount, withdrawal.asset_code, withdrawal.decimal_exponent)

  function open(next: DialogState) {
    setActionError(null)
    setDialog(next)
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={`Withdrawal ${withdrawal.id}`}
        description={`Player: ${withdrawal.player_account_id}`}
        actions={
          canDecide ? (
            <>
              <Button variant="danger" onClick={() => open('reject')}>
                Reject
              </Button>
              <Button variant="primary" onClick={() => open('approve')}>
                Approve
              </Button>
            </>
          ) : canSubmit ? (
            <Button variant="primary" onClick={() => open('submit')}>
              Submit to provider
            </Button>
          ) : canResolve ? (
            <Button variant="primary" onClick={() => open('resolve')}>
              Resolve
            </Button>
          ) : undefined
        }
      />

      {(canDecide || canSubmit || canResolve) && claims && (
        <p className="text-sm text-slate-600" data-testid="acting-as">
          Acting as staff <span className="font-mono">{claims.sub}</span> ({claims.role}).
          {canDecide && ' Each approval must come from a different person-linked finance user; you cannot approve twice.'}
        </p>
      )}

      {withdrawal.state === 'requested' && (
        <InfoNote>
          This withdrawal has not entered review yet, so it cannot be approved or rejected. Open the{' '}
          <Link to="/withdrawals?tab=pending" className="text-brand-700 underline">
            Pending review queue
          </Link>{' '}
          to move it into review.
        </InfoNote>
      )}

      {successMessage && (
        <div role="status" className="rounded-md border border-green-200 bg-green-50 px-4 py-3 text-sm text-green-800">
          {successMessage}
        </div>
      )}

      <Card title="Details">
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Amount</dt>
            <dd className="mt-1 font-medium text-slate-900">{amountLabel}</dd>
          </div>
          <div>
            <dt className="text-slate-500">State</dt>
            <dd className="mt-1">
              <Badge tone={withdrawalStatusTone(withdrawal.state)}>{withdrawal.state}</Badge>
            </dd>
          </div>
          <div>
            <dt className="text-slate-500">Requested at</dt>
            <dd className="mt-1 text-slate-700">{withdrawal.requested_at}</dd>
          </div>
          <div data-testid="bound-instrument">
            <dt className="text-slate-500">Payout instrument</dt>
            <dd className="mt-1 text-slate-700">
              {boundInstrument ? (
                <>
                  <span className="font-mono">{boundInstrument.display_mask}</span>{' '}
                  <Badge tone="neutral">{boundInstrument.rail}</Badge>
                </>
              ) : (
                <span className="text-slate-500">None bound (legacy withdrawal)</span>
              )}
            </dd>
          </div>
          {withdrawal.hold_ledger_transaction_id && (
            <div>
              <dt className="text-slate-500">Hold ledger transaction</dt>
              <dd className="mt-1 break-all font-mono text-xs text-slate-700">{withdrawal.hold_ledger_transaction_id}</dd>
            </div>
          )}
        </dl>
      </Card>

      <Link to="/withdrawals" className="text-sm text-brand-700 hover:underline">
        &larr; Back to withdrawals
      </Link>

      {dialog === 'approve' && (
        <ConfirmDialog
          title="Approve withdrawal"
          description={`Approve payout of ${amountLabel} to this player? This moves real money. The server independently enforces four-eyes approval, distinct-approver, and beneficiary-separation rules - this confirmation does not bypass any of them.`}
          confirmLabel="Approve withdrawal"
          isSubmitting={approveMutation.isPending}
          errorMessage={actionError}
          onConfirm={() => approveGuard.run(() => approveMutation.mutate())}
          onCancel={closeDialog}
        />
      )}
      {dialog === 'reject' && (
        <ConfirmDialog
          title="Reject withdrawal"
          description="This releases the hold back to the player's wallet and marks the withdrawal rejected. A reason code is required."
          variant="danger"
          confirmLabel="Reject withdrawal"
          requireReason
          reasonLabel="Reason code"
          isSubmitting={rejectMutation.isPending}
          errorMessage={actionError}
          onConfirm={(reason) => rejectGuard.run(() => rejectMutation.mutate(reason))}
          onCancel={closeDialog}
        />
      )}
      {dialog === 'submit' && (
        <Modal title="Submit withdrawal" onClose={closeDialog}>
          <form
            className="flex flex-col gap-4"
            noValidate
            onSubmit={(e) => {
              e.preventDefault()
              const method = effectiveMethod
              if (!method) return
              submitGuard.run(() => submitMutation.mutate(method))
            }}
          >
            <p className="text-sm text-slate-600">
              Send the approved payout of {amountLabel} to a payment provider. The provider is routed by the tenant&apos;s payment
              capability configuration for this asset and payment method.
            </p>
            {boundInstrument ? (
              <TextInput
                label="Payment method"
                value={boundInstrument.rail}
                readOnly
                help={`Taken from the bound payout instrument ${boundInstrument.display_mask}; it cannot be changed here.`}
              />
            ) : (
              <TextInput
                label="Payment method"
                value={paymentMethod}
                onChange={(e) => setPaymentMethod(e.target.value)}
                help="This withdrawal has no bound payout instrument (legacy); enter the payment method."
                required
              />
            )}
            {actionError && <p className="text-sm text-red-600">{actionError}</p>}
            <div className="flex justify-end gap-2">
              <Button type="button" variant="secondary" onClick={closeDialog} disabled={submitMutation.isPending}>
                Cancel
              </Button>
              <Button type="submit" disabled={!effectiveMethod} isLoading={submitMutation.isPending}>
                Submit withdrawal
              </Button>
            </div>
          </form>
        </Modal>
      )}
      {dialog === 'resolve' && (
        <ConfirmDialog
          title="Resolve withdrawal"
          description="Query the provider this withdrawal was submitted to and complete or fail it according to the provider's answer. This never resubmits the payout."
          confirmLabel="Resolve withdrawal"
          isSubmitting={resolveMutation.isPending}
          errorMessage={actionError}
          onConfirm={() => resolveGuard.run(() => resolveMutation.mutate())}
          onCancel={closeDialog}
        />
      )}
    </div>
  )
}
