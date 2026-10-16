import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { createMyTimeOut, type RgOptions } from '../../api/rg'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Select } from '../../components/Select'
import { formatHours, formatWhen, rgErrorMessage, useAttemptKey } from './rgSupport'

/**
 * Take a time-out: a bounded break that cannot be ended early. The durations are the server's
 * (GET /v1/me/rg/options). Two steps - choose, then confirm - because it cannot be undone.
 */
export function TimeOutCard({ options }: { options: RgOptions }) {
  const queryClient = useQueryClient()
  const attempt = useAttemptKey()
  const [hours, setHours] = useState<string>('')
  const [confirming, setConfirming] = useState(false)

  const mutation = useMutation({
    mutationFn: (h: number) => createMyTimeOut(h, attempt.keyFor(`time-out:${h}`)),
    onSuccess: async () => {
      attempt.reset()
      setConfirming(false)
      setHours('')
      await queryClient.invalidateQueries({ queryKey: ['rg-status'] })
    },
  })

  const choices = options.time_out_hours
  const selected = hours === '' ? null : Number(hours)

  return (
    <Card title="Take a time-out">
      <div className="flex flex-col gap-3 text-sm">
        <p className="text-slate-600">
          A time-out is a break from deposits and play. It <strong>cannot be ended early</strong>. You can still withdraw
          your funds while it runs.
        </p>
        {choices.length === 0 ? (
          <p className="text-slate-600">No time-out durations are available right now.</p>
        ) : (
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1">
              <span className="text-slate-600">Duration</span>
              <Select
                aria-label="Time-out duration"
                value={hours}
                disabled={confirming}
                onChange={(e) => setHours(e.target.value)}
                placeholder="Choose..."
                options={choices.map((h) => ({ value: String(h), label: formatHours(h) }))}
              />
            </label>
            {!confirming && (
              <Button variant="secondary" disabled={selected === null} onClick={() => setConfirming(true)}>
                Review time-out
              </Button>
            )}
          </div>
        )}
        {confirming && selected !== null && (
          <div className="flex flex-col gap-2 rounded-md border border-amber-200 bg-amber-50 p-3" role="group" aria-label="Confirm time-out">
            <p className="text-amber-900">
              You are about to start a <strong>{formatHours(selected)}</strong> time-out. You will not be able to deposit or
              play until it ends, and it cannot be cancelled.
            </p>
            <div className="flex gap-2">
              <Button variant="danger" isLoading={mutation.isPending} onClick={() => mutation.mutate(selected)}>
                Start time-out
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
        {mutation.isSuccess && mutation.data && (
          <p role="status" className="text-emerald-700">
            Your time-out has started{mutation.data.ends_at ? ` and ends ${formatWhen(mutation.data.ends_at)}` : ''}.
          </p>
        )}
      </div>
    </Card>
  )
}
