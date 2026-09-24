import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { upsertCasinoGame, type CasinoGame } from '../../api/casinoAdmin'
import { ErrorAlert, InfoNote } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Checkbox, parseList, SelectField, TextInput } from '../../components/FormField'
import { PageHeader } from '../../components/PageHeader'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'

const STATUS_OPTIONS = [
  { value: 'active', label: 'active' },
  { value: 'disabled', label: 'disabled' },
]

/**
 * PUT /v1/admin/casino/games - platform_admin only (PermCasinoCatalogueManage),
 * NOT tenant-scoped. Upserts by (provider_id, provider_game_id). There is no
 * admin catalogue list API, so the returned game id is shown prominently.
 */
export function CasinoCataloguePage() {
  const guard = useOnceGuard()
  const [providerId, setProviderId] = useState('mock-casino')
  const [providerGameId, setProviderGameId] = useState('mock-slot-001')
  const [name, setName] = useState('Mock Fortune Slot')
  const [gameType, setGameType] = useState('slot')
  const [rtpVariant, setRtpVariant] = useState('')
  const [volatility, setVolatility] = useState('')
  const [featureFlags, setFeatureFlags] = useState('')
  const [assets, setAssets] = useState('USD')
  const [blocklist, setBlocklist] = useState('')
  const [mobile, setMobile] = useState(true)
  const [demo, setDemo] = useState(true)
  const [status, setStatus] = useState<'active' | 'disabled'>('active')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [saved, setSaved] = useState<CasinoGame | null>(null)

  const incomplete = !providerId.trim() || !providerGameId.trim() || !name.trim() || !gameType.trim()

  const mutation = useMutation({
    mutationFn: () => {
      const flags = parseList(featureFlags)
      const supported = parseList(assets)
      const blocked = parseList(blocklist)
      return upsertCasinoGame({
        provider_id: providerId.trim(),
        provider_game_id: providerGameId.trim(),
        name: name.trim(),
        game_type: gameType.trim(),
        ...(rtpVariant.trim() ? { rtp_variant: rtpVariant.trim() } : {}),
        ...(volatility.trim() ? { volatility: volatility.trim() } : {}),
        ...(flags.length ? { feature_flags: flags } : {}),
        ...(supported.length ? { supported_assets: supported } : {}),
        mobile_supported: mobile,
        demo_supported: demo,
        ...(blocked.length ? { jurisdiction_blocklist: blocked } : {}),
        status,
      })
    },
    onSuccess: (game) => {
      setError(null)
      setSaved(game)
    },
    onError: (err) => setError(describeError(err, 'Failed to upsert catalogue game.')),
    onSettled: () => guard.release(),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setSaved(null)
    guard.run(() => mutation.mutate())
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="Casino Catalogue" description="Register or update a title in the platform-wide casino game catalogue." />
      <Card title="Upsert catalogue game">
        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <InfoNote>
            The catalogue cannot be listed from the Back Office (no admin list API). Upserting an existing provider/game pair
            updates it and returns the same Game ID. Removing a jurisdiction block or re-activating a disabled game requires an
            approved catalogue change request and is refused otherwise.
          </InfoNote>
          <div className="grid gap-4 sm:grid-cols-2">
            <TextInput label="Provider ID" value={providerId} onChange={(e) => setProviderId(e.target.value)} required />
            <TextInput label="Provider game ID" value={providerGameId} onChange={(e) => setProviderGameId(e.target.value)} required />
            <TextInput label="Game name" value={name} onChange={(e) => setName(e.target.value)} required />
            <TextInput label="Game type" value={gameType} onChange={(e) => setGameType(e.target.value)} help="e.g. slot, table, live" required />
            <TextInput label="RTP variant (optional)" value={rtpVariant} onChange={(e) => setRtpVariant(e.target.value)} />
            <TextInput label="Volatility (optional)" value={volatility} onChange={(e) => setVolatility(e.target.value)} />
            <TextInput label="Feature flags (optional)" value={featureFlags} onChange={(e) => setFeatureFlags(e.target.value)} />
            <TextInput label="Supported assets" value={assets} onChange={(e) => setAssets(e.target.value)} help="Comma-separated, e.g. EUR, USD" />
            <TextInput
              label="Jurisdiction blocklist (optional)"
              value={blocklist}
              onChange={(e) => setBlocklist(e.target.value)}
              help="Comma-separated jurisdiction codes."
            />
            <SelectField label="Game status" options={STATUS_OPTIONS} value={status} onChange={(e) => setStatus(e.target.value as 'active' | 'disabled')} />
          </div>
          <div className="flex flex-wrap gap-6">
            <Checkbox label="Mobile supported" checked={mobile} onChange={(e) => setMobile(e.target.checked)} />
            <Checkbox label="Demo supported" checked={demo} onChange={(e) => setDemo(e.target.checked)} />
          </div>
          <ErrorAlert error={error} />
          {saved && (
            <div role="status" className="rounded-md border border-green-300 bg-green-50 p-4 text-sm text-green-900">
              <p className="font-semibold">
                Game &quot;{saved.name}&quot; ({saved.provider_id}) saved.
              </p>
              <p className="mt-2 text-green-700">Game ID</p>
              <p className="break-all font-mono text-base">{saved.id}</p>
              <p className="mt-2">Give this Game ID to the tenant admin to enable availability (Providers &amp; Games page).</p>
            </div>
          )}
          <div>
            <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
              Upsert game
            </Button>
          </div>
        </form>
      </Card>
    </div>
  )
}
