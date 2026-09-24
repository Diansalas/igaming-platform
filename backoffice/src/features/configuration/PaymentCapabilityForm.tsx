import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { writePaymentCapability, type AmountLimit } from '../../api/providerConfig'
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

interface LimitRow {
  asset_code: string
  min_amount: string
  max_amount: string
}

/**
 * PUT /v1/admin/providers/{providerID}/capability - tenant_admin only
 * (PermProviderConfigWrite + RequireTenantScope). Write-only: the API has
 * no GET for this resource. Defaults below are just form defaults; the
 * backend's narrowing check against the adapter's declared capability is
 * authoritative and its 400 message is shown verbatim.
 */
export function PaymentCapabilityForm() {
  const guard = useOnceGuard()
  const [providerId, setProviderId] = useState('mock-payments')
  const [fiat, setFiat] = useState('USD')
  const [crypto, setCrypto] = useState('')
  const [methods, setMethods] = useState('card')
  const [countries, setCountries] = useState('')
  const [deposit, setDeposit] = useState(true)
  const [withdrawal, setWithdrawal] = useState(true)
  const [refund, setRefund] = useState(false)
  const [priority, setPriority] = useState('1')
  const [status, setStatus] = useState<'active' | 'disabled'>('active')
  const [limits, setLimits] = useState<LimitRow[]>([])
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [saved, setSaved] = useState<{ id: string; providerId: string } | null>(null)

  const limitsInvalid = limits.some(
    (l) => !l.asset_code.trim() || !/^\d+$/.test(l.min_amount) || !/^\d+$/.test(l.max_amount),
  )
  const priorityInvalid = !/^-?\d+$/.test(priority.trim())
  const incomplete = !providerId.trim() || priorityInvalid || limitsInvalid

  const mutation = useMutation({
    mutationFn: (pid: string) =>
      writePaymentCapability(pid, {
        supported_fiat_currencies: parseList(fiat),
        supported_crypto_assets: parseList(crypto),
        supported_payment_methods: parseList(methods),
        supported_countries: parseList(countries),
        supports_deposit: deposit,
        supports_withdrawal: withdrawal,
        supports_refund_reversal: refund,
        amount_limits: limits.map<AmountLimit>((l) => ({
          asset_code: l.asset_code.trim(),
          min_amount: Number(l.min_amount),
          max_amount: Number(l.max_amount),
        })),
        priority: Number(priority.trim()),
        status,
      }),
    onSuccess: (res, pid) => {
      setError(null)
      setSaved({ id: res.id, providerId: pid })
    },
    onError: (err) => setError(describeError(err, 'Failed to save payment provider capability.')),
    onSettled: () => guard.release(),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setSaved(null)
    const pid = providerId.trim()
    guard.run(() => mutation.mutate(pid))
  }

  function updateLimit(i: number, patch: Partial<LimitRow>) {
    setLimits((rows) => rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)))
  }

  return (
    <Card title="Payment provider capability">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <InfoNote>
          Current configuration cannot be read back (no read API). Saving replaces this tenant&apos;s configuration for the
          provider. Lists are comma-separated; an empty list stores &quot;none&quot; (countries: empty means unrestricted).
        </InfoNote>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput label="Payment provider ID" value={providerId} onChange={(e) => setProviderId(e.target.value)} required />
          <SelectField
            label="Payment capability status"
            options={STATUS_OPTIONS}
            value={status}
            onChange={(e) => setStatus(e.target.value as 'active' | 'disabled')}
          />
          <TextInput label="Supported fiat currencies" value={fiat} onChange={(e) => setFiat(e.target.value)} help="e.g. EUR, USD" />
          <TextInput label="Supported crypto assets" value={crypto} onChange={(e) => setCrypto(e.target.value)} />
          <TextInput
            label="Supported payment methods"
            value={methods}
            onChange={(e) => setMethods(e.target.value)}
            help="e.g. card, bank_transfer"
          />
          <TextInput
            label="Supported countries"
            value={countries}
            onChange={(e) => setCountries(e.target.value)}
            help="ISO codes; leave empty for unrestricted."
          />
          <TextInput
            label="Payment priority"
            inputMode="numeric"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
            help={priorityInvalid ? <span className="text-red-600">Priority must be an integer.</span> : 'Lower number routes first.'}
          />
        </div>
        <div className="flex flex-wrap gap-6">
          <Checkbox label="Supports deposit" checked={deposit} onChange={(e) => setDeposit(e.target.checked)} />
          <Checkbox label="Supports withdrawal" checked={withdrawal} onChange={(e) => setWithdrawal(e.target.checked)} />
          <Checkbox label="Supports refund/reversal" checked={refund} onChange={(e) => setRefund(e.target.checked)} />
        </div>
        <div className="flex flex-col gap-2">
          <p className="text-sm font-medium text-slate-700">Amount limits (optional, integer minor units)</p>
          {limits.map((l, i) => (
            <div key={i} className="grid items-end gap-2 sm:grid-cols-4">
              <TextInput label={`Limit ${i + 1} asset code`} value={l.asset_code} onChange={(e) => updateLimit(i, { asset_code: e.target.value })} />
              <TextInput
                label={`Limit ${i + 1} min amount`}
                inputMode="numeric"
                value={l.min_amount}
                onChange={(e) => updateLimit(i, { min_amount: e.target.value })}
              />
              <TextInput
                label={`Limit ${i + 1} max amount`}
                inputMode="numeric"
                value={l.max_amount}
                onChange={(e) => updateLimit(i, { max_amount: e.target.value })}
              />
              <Button type="button" variant="ghost" onClick={() => setLimits((rows) => rows.filter((_, idx) => idx !== i))}>
                Remove limit {i + 1}
              </Button>
            </div>
          ))}
          {limitsInvalid && <p className="text-sm text-red-600">Each limit needs an asset code and whole-number min/max amounts.</p>}
          <div>
            <Button
              type="button"
              variant="secondary"
              onClick={() => setLimits((rows) => [...rows, { asset_code: '', min_amount: '', max_amount: '' }])}
            >
              Add amount limit
            </Button>
          </div>
        </div>
        <ErrorAlert error={error} />
        {saved && (
          <SuccessAlert>
            Payment capability for <span className="font-mono">{saved.providerId}</span> saved. Capability ID:{' '}
            <span className="font-mono">{saved.id}</span>
          </SuccessAlert>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Save payment capability
          </Button>
        </div>
      </form>
    </Card>
  )
}
