import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { writeCasinoCapability } from '../../api/casinoAdmin'
import { ErrorAlert, InfoNote, SuccessAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Checkbox, parseList, SelectField, TextInput } from '../../components/FormField'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'

const STATUS_OPTIONS = [
  { value: 'active', label: 'active' },
  { value: 'disabled', label: 'disabled' },
]

const FLAGS = [
  ['supports_catalogue', 'Supports catalogue'],
  ['supports_launch', 'Supports launch'],
  ['supports_balance', 'Supports balance'],
  ['supports_bet', 'Supports bet'],
  ['supports_win', 'Supports win'],
  ['supports_rollback', 'Supports rollback'],
] as const
type FlagKey = (typeof FLAGS)[number][0]

/**
 * PUT /v1/admin/casino/providers/{providerID}/capability - tenant_admin only
 * (PermCasinoConfigWrite + RequireTenantScope). Write-only (no read API).
 */
export function CasinoCapabilityForm() {
  const guard = useOnceGuard()
  const [providerId, setProviderId] = useState('mock-casino')
  const [flags, setFlags] = useState<Record<FlagKey, boolean>>({
    supports_catalogue: true,
    supports_launch: true,
    supports_balance: true,
    supports_bet: true,
    supports_win: true,
    supports_rollback: true,
  })
  const [assets, setAssets] = useState('USD')
  const [gameTypes, setGameTypes] = useState('slot')
  const [priority, setPriority] = useState('1')
  const [status, setStatus] = useState<'active' | 'disabled'>('active')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [saved, setSaved] = useState<{ id: string; providerId: string } | null>(null)

  const priorityInvalid = !/^-?\d+$/.test(priority.trim())
  const incomplete = !providerId.trim() || priorityInvalid

  const mutation = useMutation({
    mutationFn: (pid: string) =>
      writeCasinoCapability(pid, {
        ...flags,
        supported_assets: parseList(assets),
        supported_game_types: parseList(gameTypes),
        priority: Number(priority.trim()),
        status,
      }),
    onSuccess: (res, pid) => {
      setError(null)
      setSaved({ id: res.id, providerId: pid })
    },
    onError: (err) => setError(describeError(err, 'Failed to save casino provider capability.')),
    onSettled: () => guard.release(),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setSaved(null)
    const pid = providerId.trim()
    guard.run(() => mutation.mutate(pid))
  }

  return (
    <Card title="Casino provider capability">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <InfoNote>
          Current configuration cannot be read back (no read API). Saving replaces this tenant&apos;s configuration for the
          provider. Lists are comma-separated.
        </InfoNote>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput label="Casino provider ID" value={providerId} onChange={(e) => setProviderId(e.target.value)} required />
          <SelectField
            label="Casino capability status"
            options={STATUS_OPTIONS}
            value={status}
            onChange={(e) => setStatus(e.target.value as 'active' | 'disabled')}
          />
          <TextInput label="Casino supported assets" value={assets} onChange={(e) => setAssets(e.target.value)} help="e.g. EUR, USD" />
          <TextInput
            label="Supported game types"
            value={gameTypes}
            onChange={(e) => setGameTypes(e.target.value)}
            help="e.g. slot, table, live"
          />
          <TextInput
            label="Casino priority"
            inputMode="numeric"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
            help={priorityInvalid ? <span className="text-red-600">Priority must be an integer.</span> : undefined}
          />
        </div>
        <div className="flex flex-wrap gap-6">
          {FLAGS.map(([key, label]) => (
            <Checkbox key={key} label={label} checked={flags[key]} onChange={(e) => setFlags((f) => ({ ...f, [key]: e.target.checked }))} />
          ))}
        </div>
        <ErrorAlert error={error} />
        {saved && (
          <SuccessAlert>
            Casino capability for <span className="font-mono">{saved.providerId}</span> saved. Capability ID:{' '}
            <span className="font-mono">{saved.id}</span>
          </SuccessAlert>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Save casino capability
          </Button>
        </div>
      </form>
    </Card>
  )
}
