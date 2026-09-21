import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { approveWithdrawal, getWithdrawal, rejectWithdrawal } from '../../api/withdrawals'
import { ApiError } from '../../api/types'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { formatMoney } from '../../lib/money'
import { useOnceGuard } from '../../lib/useOnceGuard'
import { withdrawalStatusTone } from './status'

type DialogState = 'approve' | 'reject' | null

/**
 * This page moves real money: every action here requires an explicit
 * confirmation dialog (never a bare button that fires the mutation), and
 * the four-eyes/distinct-approver/beneficiary/threshold rules are entirely
 * server-side (internal/withdrawal.Approve/Reject) - this page never
 * computes whether an approval "should" succeed, it only renders what the
 * server returns and surfaces exactly the error the server gives back if
 * it refuses (e.g. self-approval, duplicate decision, not-linked approver).
 */
export function WithdrawalDetailPage() {
  const { id = '' } = useParams()
  const queryClient = useQueryClient()
  const [dialog, setDialog] = useState<DialogState>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [successMessage, setSuccessMessage] = useState<string | null>(null)
  // This page moves real money via a distinct-approver/four-eyes-enforced
  // endpoint - a stray double-submit (see useOnceGuard's doc comment) is a
  // genuinely serious operational risk here, not just a UX nuisance, so
  // approve/reject each get their own synchronous once-guard on top of
  // ConfirmDialog's own isSubmitting-disables-the-button behavior.
  const approveGuard = useOnceGuard()
  const rejectGuard = useOnceGuard()

  const query = useQuery({
    queryKey: ['withdrawal', id],
    queryFn: () => getWithdrawal(id),
    enabled: !!id,
  })

  const approveMutation = useMutation({
    mutationFn: () => approveWithdrawal(id),
    onSuccess: async (result) => {
      setDialog(null)
      setActionError(null)
      setSuccessMessage(result.approved ? 'Withdrawal approved.' : 'Decision recorded; a second approval is still required.')
      await queryClient.invalidateQueries({ queryKey: ['withdrawal', id] })
      await queryClient.invalidateQueries({ queryKey: ['withdrawals'] })
    },
    onError: (err) => setActionError(err instanceof ApiError ? err.message : 'Failed to approve this withdrawal.'),
    onSettled: () => approveGuard.release(),
  })

  const rejectMutation = useMutation({
    mutationFn: (reasonCode: string) => rejectWithdrawal(id, reasonCode),
    onSuccess: async () => {
      setDialog(null)
      setActionError(null)
      setSuccessMessage('Withdrawal rejected.')
      await queryClient.invalidateQueries({ queryKey: ['withdrawal', id] })
      await queryClient.invalidateQueries({ queryKey: ['withdrawals'] })
    },
    onError: (err) => setActionError(err instanceof ApiError ? err.message : 'Failed to reject this withdrawal.'),
    onSettled: () => rejectGuard.release(),
  })

  if (query.isLoading) return <LoadingSpinner label="Loading withdrawal..." />
  if (query.error) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  const withdrawal = query.data
  if (!withdrawal) return null

  const canDecide = withdrawal.state === 'pending_review' || withdrawal.state === 'requested'

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={`Withdrawal ${withdrawal.id}`}
        description={`Player: ${withdrawal.player_account_id}`}
        actions={
          canDecide ? (
            <>
              <Button
                variant="danger"
                onClick={() => {
                  setActionError(null)
                  setDialog('reject')
                }}
              >
                Reject
              </Button>
              <Button
                variant="primary"
                onClick={() => {
                  setActionError(null)
                  setDialog('approve')
                }}
              >
                Approve
              </Button>
            </>
          ) : undefined
        }
      />

      {successMessage && (
        <div className="rounded-md border border-green-200 bg-green-50 px-4 py-3 text-sm text-green-800">{successMessage}</div>
      )}

      <Card title="Details">
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Amount</dt>
            <dd className="mt-1 font-medium text-slate-900">
              {formatMoney(withdrawal.amount, withdrawal.asset_code, withdrawal.decimal_exponent)}
            </dd>
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
          description={`Approve payout of ${formatMoney(withdrawal.amount, withdrawal.asset_code, withdrawal.decimal_exponent)} to this player? This moves real money. The server independently enforces four-eyes approval, distinct-approver, and beneficiary-separation rules - this confirmation does not bypass any of them.`}
          confirmLabel="Approve withdrawal"
          isSubmitting={approveMutation.isPending}
          errorMessage={actionError}
          onConfirm={() => approveGuard.run(() => approveMutation.mutate())}
          onCancel={() => {
            setDialog(null)
            setActionError(null)
          }}
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
          onCancel={() => {
            setDialog(null)
            setActionError(null)
          }}
        />
      )}
    </div>
  )
}
