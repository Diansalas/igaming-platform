import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { ApiError } from '../../api/types'
import { listMyWallets } from '../../api/wallet'
import { cancelWithdrawal, listMyWithdrawals, requestWithdrawal, type WithdrawalState } from '../../api/withdrawals'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { TextInput } from '../../components/TextInput'
import { newIdempotencyKey } from '../../lib/idempotencyKey'
import { formatMoney, toMinorUnits } from '../../lib/money'

const STATE_LABEL: Record<WithdrawalState, string> = {
  requested: 'Requested',
  pending_review: 'Awaiting review',
  approved: 'Approved',
  rejected: 'Rejected',
  submitted: 'Sent to payment provider',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled',
  reversed: 'Reversed',
}

function stateTone(state: WithdrawalState): BadgeTone {
  switch (state) {
    case 'completed':
      return 'success'
    case 'rejected':
    case 'failed':
    case 'reversed':
      return 'danger'
    case 'cancelled':
      return 'neutral'
    default:
      return 'warning'
  }
}

function requestErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.message === 'insufficient available balance') return 'Your available balance does not cover this amount.'
    return err.message
  }
  return 'Could not request this withdrawal.'
}

/**
 * Player withdrawal requests through POST /v1/me/withdrawals. The server
 * places the amount on hold and the request then goes through the
 * operator's four-eyes review in the Back Office (two distinct approvers)
 * before it is submitted to the payment provider. This page only requests,
 * lists and - while still `requested` - cancels; every state shown comes
 * from the server, and balances are re-read after each change.
 */
export function WithdrawalPage() {
  const queryClient = useQueryClient()
  const walletsQuery = useQuery({ queryKey: ['wallets'], queryFn: listMyWallets })
  const withdrawalsQuery = useQuery({ queryKey: ['withdrawals'], queryFn: listMyWithdrawals })

  const [assetCode, setAssetCode] = useState('')
  const [amountInput, setAmountInput] = useState('')
  const [formError, setFormError] = useState<string | null>(null)
  const [lastRequestId, setLastRequestId] = useState<string | null>(null)
  // One key per withdrawal ATTEMPT: reused on retry after an error (the
  // first POST may have committed), replaced once the server has answered.
  const [idempotencyKey, setIdempotencyKey] = useState(() => newIdempotencyKey())

  const wallets = walletsQuery.data ?? []
  const selectedAsset = assetCode || wallets[0]?.asset_code || ''
  const selectedWallet = wallets.find((w) => w.asset_code === selectedAsset)

  async function refresh() {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['withdrawals'] }),
      queryClient.invalidateQueries({ queryKey: ['wallets'] }),
    ])
  }

  const requestMutation = useMutation({
    mutationFn: requestWithdrawal,
    onSuccess: async (wr) => {
      setLastRequestId(wr.id)
      setAmountInput('')
      setFormError(null)
      setIdempotencyKey(newIdempotencyKey())
      await refresh()
    },
    onError: (err) => setFormError(requestErrorMessage(err)),
  })

  const cancelMutation = useMutation({ mutationFn: cancelWithdrawal, onSettled: refresh })

  function onSubmit() {
    if (requestMutation.isPending) return
    if (!selectedAsset) {
      setFormError('You have no wallet yet - make a deposit first.')
      return
    }
    const amount = toMinorUnits(amountInput, selectedAsset)
    if (amount === null || amount <= 0) {
      setFormError('Enter a valid amount.')
      return
    }
    setFormError(null)
    requestMutation.mutate({ assetCode: selectedAsset, amount, idempotencyKey })
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Withdraw"
        description="Withdrawals are reviewed and approved by two operators before they are paid out."
      />

      <Card title="New withdrawal">
        {walletsQuery.isLoading && <LoadingSpinner label="Loading wallets..." />}
        {walletsQuery.error && <ErrorState error={walletsQuery.error} onRetry={() => void walletsQuery.refetch()} />}
        {walletsQuery.data && wallets.length === 0 && (
          <EmptyState message="No wallets yet - make a deposit before withdrawing." />
        )}
        {wallets.length > 0 && (
          <div className="flex flex-col gap-4 sm:max-w-sm">
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium text-slate-700">Asset</span>
              <Select
                value={selectedAsset}
                onChange={(e) => setAssetCode(e.target.value)}
                options={wallets.map((w) => ({ value: w.asset_code, label: w.asset_code }))}
              />
            </label>
            {selectedWallet && (
              <dl className="grid grid-cols-2 gap-2 text-sm">
                <div>
                  <dt className="text-xs text-slate-500">Available to withdraw</dt>
                  <dd className="font-medium">{formatMoney(selectedWallet.available_balance, selectedWallet.asset_code)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-slate-500">Held for withdrawal</dt>
                  <dd>{formatMoney(selectedWallet.held_for_withdrawal, selectedWallet.asset_code)}</dd>
                </div>
              </dl>
            )}
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium text-slate-700">Amount ({selectedAsset})</span>
              <TextInput
                type="number"
                min="0"
                step="0.01"
                inputMode="decimal"
                value={amountInput}
                onChange={(e) => setAmountInput(e.target.value)}
                placeholder="25.00"
              />
            </label>
            {formError && (
              <p role="alert" className="text-sm text-red-600">
                {formError}
              </p>
            )}
            <Button isLoading={requestMutation.isPending} onClick={onSubmit}>
              Request withdrawal
            </Button>
            {lastRequestId && !requestMutation.isPending && (
              <p role="status" className="text-sm text-emerald-700">
                Withdrawal requested. The amount is on hold while operators review it.
              </p>
            )}
          </div>
        )}
      </Card>

      <Card title="My withdrawals">
        {withdrawalsQuery.isLoading && <LoadingSpinner label="Loading withdrawals..." />}
        {withdrawalsQuery.error && (
          <ErrorState error={withdrawalsQuery.error} onRetry={() => void withdrawalsQuery.refetch()} />
        )}
        {cancelMutation.error && (
          <p role="alert" className="mb-2 text-sm text-red-600">
            {cancelMutation.error instanceof ApiError ? cancelMutation.error.message : 'Could not cancel this withdrawal.'}
          </p>
        )}
        {withdrawalsQuery.data && withdrawalsQuery.data.length === 0 && <EmptyState message="No withdrawals yet." />}
        {withdrawalsQuery.data && withdrawalsQuery.data.length > 0 && (
          <ul className="divide-y divide-border">
            {withdrawalsQuery.data.map((w) => (
              <li key={w.id} className="flex flex-wrap items-center justify-between gap-2 py-2 text-sm">
                <span>
                  {formatMoney(w.amount, w.asset_code)}{' '}
                  <span className="text-xs text-slate-500">{new Date(w.requested_at).toLocaleString()}</span>
                </span>
                <span className="flex items-center gap-2">
                  {w.state === 'requested' && (
                    <Button
                      variant="ghost"
                      isLoading={cancelMutation.isPending && cancelMutation.variables === w.id}
                      disabled={cancelMutation.isPending}
                      onClick={() => cancelMutation.mutate(w.id)}
                    >
                      Cancel
                    </Button>
                  )}
                  <Badge tone={stateTone(w.state)}>{STATE_LABEL[w.state] ?? w.state}</Badge>
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  )
}
