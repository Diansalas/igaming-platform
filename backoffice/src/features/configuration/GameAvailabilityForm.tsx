import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { setCasinoGameAvailability } from '../../api/casinoAdmin'
import { ErrorAlert, SuccessAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Checkbox, TextInput } from '../../components/FormField'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'

/** PUT /v1/admin/casino/games/{gameID}/availability {enabled} - tenant_admin only (PermCasinoConfigWrite). */
export function GameAvailabilityForm() {
  const guard = useOnceGuard()
  const [gameId, setGameId] = useState('')
  const [enabled, setEnabled] = useState(true)
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [saved, setSaved] = useState<{ game_id: string; enabled: boolean } | null>(null)

  const mutation = useMutation({
    mutationFn: (vars: { gameId: string; enabled: boolean }) => setCasinoGameAvailability(vars.gameId, vars.enabled),
    onSuccess: (res) => {
      setError(null)
      setSaved(res)
    },
    onError: (err) => setError(describeError(err, 'Failed to set game availability.')),
    onSettled: () => guard.release(),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (!gameId.trim()) return
    setSaved(null)
    const vars = { gameId: gameId.trim(), enabled }
    guard.run(() => mutation.mutate(vars))
  }

  return (
    <Card title="Game availability">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <p className="text-sm text-slate-500">
          Opt this tenant into (or out of) a platform catalogue game. The Game ID is the UUID the platform administrator received
          when upserting the game in the Casino Catalogue.
        </p>
        <div className="grid items-end gap-4 sm:grid-cols-2">
          <TextInput label="Game ID" value={gameId} onChange={(e) => setGameId(e.target.value)} required />
          <Checkbox label="Enabled for this tenant" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        </div>
        <ErrorAlert error={error} />
        {saved && (
          <SuccessAlert>
            Game <span className="font-mono">{saved.game_id}</span> is now {saved.enabled ? 'enabled' : 'disabled'} for this tenant.
          </SuccessAlert>
        )}
        <div>
          <Button type="submit" disabled={!gameId.trim()} isLoading={mutation.isPending}>
            Save availability
          </Button>
        </div>
      </form>
    </Card>
  )
}
