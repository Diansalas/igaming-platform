import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { createTenant, type LicensingModel, type Tenant } from '../../api/tenants'
import { ErrorAlert, SuccessAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { SelectField, TextInput } from '../../components/FormField'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'

const LICENSING_OPTIONS: { value: LicensingModel; label: string }[] = [
  { value: 'under_platform_licence', label: 'Under platform licence' },
  { value: 'own_licence', label: 'Own licence' },
]

/** POST /v1/admin/tenants - platform_admin only (PermTenantWrite). */
export function CreateTenantForm() {
  const queryClient = useQueryClient()
  const guard = useOnceGuard()
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [licensingModel, setLicensingModel] = useState<LicensingModel>('under_platform_licence')
  const [reasonCode, setReasonCode] = useState('')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [created, setCreated] = useState<Tenant | null>(null)

  const mutation = useMutation({
    mutationFn: () =>
      createTenant({ name: name.trim(), slug: slug.trim(), licensing_model: licensingModel, reason_code: reasonCode.trim() }),
    onSuccess: async (tenant) => {
      setError(null)
      setCreated(tenant)
      setName('')
      setSlug('')
      setReasonCode('')
      await queryClient.invalidateQueries({ queryKey: ['tenants'] })
    },
    onError: (err) => setError(describeError(err, 'Failed to create tenant.')),
    onSettled: () => guard.release(),
  })

  const incomplete = !name.trim() || !slug.trim() || !reasonCode.trim()

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setCreated(null)
    guard.run(() => mutation.mutate())
  }

  return (
    <Card title="Create tenant">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput label="Tenant name" value={name} onChange={(e) => setName(e.target.value)} required />
          <TextInput label="Tenant slug" value={slug} onChange={(e) => setSlug(e.target.value)} required />
          <SelectField
            label="Licensing model"
            options={LICENSING_OPTIONS}
            value={licensingModel}
            onChange={(e) => setLicensingModel(e.target.value as LicensingModel)}
          />
          <TextInput
            label="Reason code"
            value={reasonCode}
            onChange={(e) => setReasonCode(e.target.value)}
            help="Required - recorded in the audit log."
            required
          />
        </div>
        <ErrorAlert error={error} />
        {created && (
          <SuccessAlert>
            Tenant <span className="font-medium">{created.name}</span> created. Tenant ID:{' '}
            <span className="font-mono">{created.id}</span>.{' '}
            <Link to={`/tenants/${created.id}`} className="underline">
              Open tenant
            </Link>
          </SuccessAlert>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Create tenant
          </Button>
        </div>
      </form>
    </Card>
  )
}
