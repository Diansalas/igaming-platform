import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { getMyDeposit, initiateDeposit, listMyDeposits, type DepositIntent, type DepositStatus } from '../../api/deposits'
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
    case 'reversed':
      return 'danger'
    case 'ambiguous':
      return 'warning'
    default:
      return 'neutral'
  }
}

/**
 * A controlled development funding mechanism, per the Stage 6 directive:
 * this calls the REAL POST /v1/me/deposits endpoint against the existing
 * internal/payments mock provider - never a shortcut that bypasses the
 * ledger. It initiates a deposit and shows exactly the intent status the
 * server returns; it never assumes success.
 *
 * DISCLOSED GAP (not a client-side workaround - see the Stage 6 report):
 * a non-magic amount always comes back `pending`, and only a correctly
 * HMAC-signed provider webhook callback (POST
 * /v1/webhooks/payments/{tenantSlug}/{providerID}) moves it to
 * `succeeded`. The mock provider's signing secret is generated in-process
 * server-side and is never exposed over HTTP, so there is no
 * browser-reachable way to complete a deposit - by design, mirroring how
 * a real PSP's webhook is never reachable from the browser either. This
 * page is honest about that rather than inventing a fake "complete now"
 * button.
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
              <p className="mt-2 text-xs text-slate-500">
                Awaiting provider confirmation. In this sandbox environment the mock provider&apos;s webhook callback is
                signed server-side and cannot be completed from the browser - a backend/QA operator must trigger it out
                of band for this deposit to credit your wallet. This page will reflect the real balance the moment it
                does.
              </p>
            )}
            {effectiveIntent.status === 'declined' && (
              <p className="mt-2 text-xs text-red-600">This deposit was declined by the sandbox provider.</p>
            )}
          </div>
        )}
      </Card>

      <Card title="Past deposits">
        {depositsQuery.isLoading && <LoadingSpinner label="Loading deposits..." />}
        {depositsQuery.error && <ErrorState error={depositsQuery.error} onRetry={() => void depositsQuery.refetch()} />}
        {depositsQuery.data && depositsQuery.data.length === 0 && <EmptyState message="No deposits yet." />}
        {depositsQuery.data && depositsQuery.data.length > 0 && (
          <ul className="divide-y divide-border">
            {depositsQuery.data.map((d) => (
              <li key={d.id} className="flex items-center justify-between py-2 text-sm">
                <span>{formatMoney(d.amount, d.asset_code)}</span>
                <Badge tone={statusTone(d.status)}>{d.status}</Badge>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  )
}
