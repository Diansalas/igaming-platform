import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { getMe } from '../../api/auth'
import {
  getMyDeposit,
  initiateDeposit,
  listMyDeposits,
  simulateDepositCallback,
  type DepositIntent,
  type DepositStatus,
} from '../../api/deposits'
import { ApiError } from '../../api/types'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { TextInput } from '../../components/TextInput'
import { brandConfig } from '../../config/brand'
import { newIdempotencyKey } from '../../lib/idempotencyKey'
import { formatMoney, toMinorUnits } from '../../lib/money'

function statusTone(status: DepositStatus): BadgeTone {
  switch (status) {
    case 'succeeded':
      return 'success'
    case 'declined':
    case 'failed':
      return 'danger'
    case 'ambiguous':
      return 'warning'
    default:
      return 'neutral'
  }
}

/** Maps a failed simulate-callback call to an actionable message. */
export function settleErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    // The route is only registered outside production with mock settlement
    // enabled; when absent the server answers a plain (non-JSON) 404.
    if (err.status === 404 && err.code !== 'not_found') {
      return 'Simulated provider confirmation is not available on this deployment.'
    }
    if (err.code === 'service_unavailable') return err.message
    if (err.code === 'conflict') return 'This deposit is no longer awaiting provider confirmation.'
    return err.message
  }
  return 'Could not confirm this deposit.'
}

/**
 * Deposits through the REAL POST /v1/me/deposits endpoint and the tenant's
 * configured payment provider - never a shortcut that bypasses the ledger.
 * It shows exactly the intent status the server returns; it never assumes
 * success.
 *
 * A non-magic amount comes back `pending` until the provider's signed
 * webhook arrives. Outside production (staging), the server also registers
 * POST /v1/me/deposits/{id}/simulate-callback, which has the MOCK provider
 * sign a normal "succeeded" callback for the player's own deposit and runs
 * it through the same verify-and-post pipeline as the public webhook. The
 * "Simulate provider confirmation" action calls exactly that; on a
 * production deployment the route does not exist and the action reports
 * so. The wallet balance shown afterwards is re-read from the server.
 *
 * A `declined` intent carries no reason in the player API (the reason -
 * e.g. no_routable_provider, player not active - is recorded server-side
 * in the audit log only), so the page lists the likely causes rather than
 * guessing one.
 */
export function DepositPage() {
  const queryClient = useQueryClient()
  const [assetCode, setAssetCode] = useState(brandConfig.defaultAssetCode)
  const [amountInput, setAmountInput] = useState('')
  const [paymentMethod, setPaymentMethod] = useState('card')
  const [lastIntent, setLastIntent] = useState<DepositIntent | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  // Minted once per deposit ATTEMPT, not per submit click - Stage 6.1
  // security review finding: generating a fresh key inside onSubmit meant
  // a retry after a network/timeout error (where the original POST may
  // have actually committed) placed a genuine second deposit request.
  // Reused across any retry of the SAME attempt; only regenerated once
  // this attempt actually succeeds, since the next submit is then a
  // genuinely new attempt. See BetSlipContext.tsx's identical fix.
  const [idempotencyKey, setIdempotencyKey] = useState(() => newIdempotencyKey())

  const depositsQuery = useQuery({ queryKey: ['deposits'], queryFn: listMyDeposits })
  const meQuery = useQuery({ queryKey: ['me'], queryFn: getMe })
  const [settleError, setSettleError] = useState<string | null>(null)

  const settleMutation = useMutation({
    mutationFn: simulateDepositCallback,
    onMutate: () => setSettleError(null),
    onSuccess: async (result) => {
      if (lastIntent && lastIntent.id === result.deposit_intent_id) {
        setLastIntent({ ...lastIntent, status: result.status })
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['deposits'] }),
        queryClient.invalidateQueries({ queryKey: ['deposit'] }),
        queryClient.invalidateQueries({ queryKey: ['wallets'] }),
      ])
    },
    onError: (err) => setSettleError(settleErrorMessage(err)),
  })

  // Polls the just-created intent's status - this is the same GET a page
  // reload would perform, not a shortcut: if a backend operator resolves
  // the mock webhook out of band, this reflects the real outcome without
  // the player needing to refresh.
  const pollQuery = useQuery({
    queryKey: ['deposit', lastIntent?.id],
    queryFn: () => getMyDeposit(lastIntent!.id),
    enabled: !!lastIntent && lastIntent.status === 'pending',
    refetchInterval: 4000,
  })

  const effectiveIntent = pollQuery.data ?? lastIntent

  const mutation = useMutation({
    mutationFn: initiateDeposit,
    onSuccess: async (intent) => {
      setLastIntent(intent)
      setFormError(null)
      setIdempotencyKey(newIdempotencyKey())
      await queryClient.invalidateQueries({ queryKey: ['deposits'] })
    },
    onError: (err) => setFormError(err instanceof ApiError ? err.message : 'Failed to start this deposit.'),
  })

  const amountMinorUnits = toMinorUnits(amountInput, assetCode)

  function onSubmit() {
    if (amountMinorUnits === null) {
      setFormError('Enter a valid amount.')
      return
    }
    setFormError(null)
    mutation.mutate({ assetCode, amount: amountMinorUnits, paymentMethod, idempotencyKey })
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="Deposit" description="Development funding via the sandbox payment provider." />

      {meQuery.data && meQuery.data.status !== 'active' && (
        <div role="status" className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900">
          Your account status is <strong>{meQuery.data.status}</strong>. Deposits are declined until your account is
          active.{' '}
          {meQuery.data.status === 'pending_verification' && (
            <Link to="/account" className="font-medium underline">
              Verify your email
            </Link>
          )}
        </div>
      )}

      <Card title="New deposit">
        <div className="flex flex-col gap-4 sm:max-w-sm">
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Asset</span>
            <Select
              value={assetCode}
              onChange={(e) => setAssetCode(e.target.value)}
              options={[
                { value: 'USD', label: 'USD' },
                { value: 'EUR', label: 'EUR' },
                { value: 'GBP', label: 'GBP' },
              ]}
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Amount ({assetCode})</span>
            <TextInput
              type="number"
              min="0"
              step="0.01"
              inputMode="decimal"
              value={amountInput}
              onChange={(e) => setAmountInput(e.target.value)}
              placeholder="50.00"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Payment method</span>
            <Select
              value={paymentMethod}
              onChange={(e) => setPaymentMethod(e.target.value)}
              options={[
                { value: 'card', label: 'Card (sandbox)' },
                { value: 'bank_transfer', label: 'Bank transfer (sandbox)' },
              ]}
            />
          </label>

          {formError && (
            <p role="alert" className="text-sm text-red-600">
              {formError}
            </p>
          )}

          <Button isLoading={mutation.isPending} onClick={onSubmit}>
            Start deposit
          </Button>
        </div>

        {effectiveIntent && (
          <div className="mt-6 rounded-md border border-border bg-surface-alt p-4 text-sm">
            <div className="flex items-center justify-between">
              <span className="font-medium text-slate-800">Deposit {effectiveIntent.id}</span>
              <Badge tone={statusTone(effectiveIntent.status)}>{effectiveIntent.status}</Badge>
            </div>
            <p className="mt-1 text-slate-600">{formatMoney(effectiveIntent.amount, effectiveIntent.asset_code)}</p>
            {effectiveIntent.status === 'pending' && (
              <div className="mt-3 flex flex-col gap-2">
                <p className="text-xs text-slate-500">
                  Awaiting the payment provider&apos;s confirmation. Your wallet is credited only when it arrives.
                </p>
                <div>
                  <Button
                    variant="secondary"
                    isLoading={settleMutation.isPending && settleMutation.variables === effectiveIntent.id}
                    disabled={settleMutation.isPending}
                    onClick={() => settleMutation.mutate(effectiveIntent.id)}
                  >
                    Simulate provider confirmation (staging)
                  </Button>
                </div>
                <p className="text-xs text-slate-500">
                  Staging only: asks the sandbox provider to send its normal signed confirmation. Not available in
                  production.
                </p>
              </div>
            )}
            {effectiveIntent.status === 'succeeded' && (
              <p className="mt-2 text-xs text-emerald-700">
                Deposit confirmed and credited.{' '}
                <Link to="/account" className="font-medium underline">
                  View wallet balance
                </Link>
              </p>
            )}
            {effectiveIntent.status === 'declined' && (
              <p className="mt-2 text-xs text-red-600">
                This deposit was declined. Common causes: your account is not active yet, no payment provider is
                configured for this asset and method, or the provider declined it. Support can see the exact reason.
              </p>
            )}
            {settleError && (
              <p role="alert" className="mt-2 text-xs text-red-600">
                {settleError}
              </p>
            )}
          </div>
        )}
      </Card>

      <Card title="Past deposits">
        {settleError && !effectiveIntent && (
          <p role="alert" className="mb-2 text-xs text-red-600">
            {settleError}
          </p>
        )}
        {depositsQuery.isLoading && <LoadingSpinner label="Loading deposits..." />}
        {depositsQuery.error && <ErrorState error={depositsQuery.error} onRetry={() => void depositsQuery.refetch()} />}
        {depositsQuery.data && depositsQuery.data.length === 0 && <EmptyState message="No deposits yet." />}
        {depositsQuery.data && depositsQuery.data.length > 0 && (
          <ul className="divide-y divide-border">
            {depositsQuery.data.map((d) => (
              <li key={d.id} className="flex flex-wrap items-center justify-between gap-2 py-2 text-sm">
                <span>
                  {formatMoney(d.amount, d.asset_code)} <span className="text-xs text-slate-500">{d.payment_method}</span>
                </span>
                <span className="flex items-center gap-2">
                  {d.status === 'pending' && (
                    <Button
                      variant="ghost"
                      isLoading={settleMutation.isPending && settleMutation.variables === d.id}
                      disabled={settleMutation.isPending}
                      onClick={() => settleMutation.mutate(d.id)}
                    >
                      Simulate confirmation
                    </Button>
                  )}
                  <Badge tone={statusTone(d.status)}>{d.status}</Badge>
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  )
}
