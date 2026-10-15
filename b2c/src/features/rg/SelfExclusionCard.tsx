import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { createMySelfExclusion, type RgOptions } from '../../api/rg'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Select } from '../../components/Select'
import { formatWhen, rgErrorMessage } from './rgSupport'

const INDEFINITE = 'indefinite'

/**
 * Self-exclude. The strongest control, so: choose a duration (the server's list, plus indefinite if the
 * server allows it), review a plain-language statement of what happens, tick that you understand it cannot
 * be reversed, and only then confirm. The request carries no idempotency key (the route has none); the
 * button is disabled while it is in flight and after it succeeds, so a double click cannot send it twice.
 */
export function SelfExclusionCard({ options }: { options: RgOptions }) {
  const queryClient = useQueryClient()
  const [choice, setChoice] = useState<string>('')
  const [confirming, setConfirming] = useState(false)
  const [understood, setUnderstood] = useState(false)

  const mutation = useMutation({
    mutationFn: (days: number | null) => createMySelfExclusion(days),
    onSuccess: async () => {
      setConfirming(false)
      setUnderstood(false)
      await queryClient.invalidateQueries({ queryKey: ['rg-status'] })
    },
  })

  const days = choice === '' || choice === INDEFINITE ? null : Number(choice)
  const label = choice === INDEFINITE ? 'with no end date (indefinite)' : days !== null ? `for ${days} day${days === 1 ? '' : 's'}` : ''
  const done = mutation.isSuccess

  return (
    <Card title="Self-exclusion">
      <div className="flex flex-col gap-3 text-sm">
        <p className="text-slate-600">
          Self-exclusion stops you from depositing, playing casino games and placing bets on <strong>every brand on this
          platform</strong>. <strong>It cannot be ended early</strong>, even if you ask. You can still withdraw your funds.
        </p>
        {!done && (
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1">
              <span className="text-slate-600">Exclude me</span>
              <Select
                aria-label="Self-exclusion duration"
                value={choice}
                disabled={confirming}
                onChange={(e) => setChoice(e.target.value)}
                placeholder="Choose a duration..."
                options={[
                  ...options.self_exclusion_days.map((d) => ({ value: String(d), label: `For ${d} days` })),
                  ...(options.self_exclusion_indefinite_allowed ? [{ value: INDEFINITE, label: 'Indefinitely (no end date)' }] : []),
                ]}
              />
            </label>
            {!confirming && (
              <Button variant="secondary" disabled={choice === ''} onClick={() => setConfirming(true)}>
                Review self-exclusion
              </Button>
            )}
          </div>
        )}
        {confirming && !done && (
          <div className="flex flex-col gap-3 rounded-md border border-red-200 bg-red-50 p-3" role="group" aria-label="Confirm self-exclusion">
            <p className="text-red-900">
              You are about to self-exclude <strong>{label}</strong>. From the moment you confirm you will not be able to
              deposit, play or bet on any brand. Nobody, including customer support, can end it early.
            </p>
            <label className="flex items-start gap-2 text-red-900">
              <input type="checkbox" checked={understood} onChange={(e) => setUnderstood(e.target.checked)} className="mt-1" />
              <span>I understand this cannot be reversed or shortened.</span>
            </label>
            <div className="flex gap-2">
              <Button variant="danger" disabled={!understood} isLoading={mutation.isPending} onClick={() => mutation.mutate(days)}>
                Confirm self-exclusion
              </Button>
              <Button variant="ghost" disabled={mutation.isPending} onClick={() => setConfirming(false)}>
                Go back
              </Button>
            </div>
          </div>
        )}
        {mutation.error && (
          <p role="alert" className="text-red-700">
            {rgErrorMessage(mutation.error)}
          </p>
        )}
        {done && mutation.data && (
          <p role="status" className="text-emerald-700">
            You are now self-excluded
            {mutation.data.indefinite ? ' with no end date' : mutation.data.ends_at ? ` until ${formatWhen(mutation.data.ends_at)}` : ''}.
          </p>
        )}
      </div>
    </Card>
  )
}
