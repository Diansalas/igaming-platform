import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { createBrand, type Brand } from '../../api/tenants'
import { ErrorAlert, SuccessAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { TextInput } from '../../components/FormField'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'

/** POST /v1/admin/tenants/{tenantID}/brands - PermBrandWrite (platform_admin: any tenant; tenant_admin: own tenant). */
export function CreateBrandForm({ tenantId }: { tenantId: string }) {
  const queryClient = useQueryClient()
  const guard = useOnceGuard()
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [created, setCreated] = useState<Brand | null>(null)

  const mutation = useMutation({
    mutationFn: () => createBrand(tenantId, { name: name.trim(), slug: slug.trim() }),
    onSuccess: async (brand) => {
      setError(null)
      setCreated(brand)
      setName('')
      setSlug('')
      await queryClient.invalidateQueries({ queryKey: ['tenant-brands', tenantId] })
    },
    onError: (err) => setError(describeError(err, 'Failed to create brand.')),
    onSettled: () => guard.release(),
  })

  const incomplete = !name.trim() || !slug.trim()

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setCreated(null)
    guard.run(() => mutation.mutate())
  }

  return (
    <Card title="Create brand">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput label="Brand name" value={name} onChange={(e) => setName(e.target.value)} required />
          <TextInput label="Brand slug" value={slug} onChange={(e) => setSlug(e.target.value)} required />
        </div>
        <ErrorAlert error={error} />
        {created && (
          <SuccessAlert>
            Brand <span className="font-medium">{created.name}</span> created. Brand ID:{' '}
            <span className="font-mono">{created.id}</span>
          </SuccessAlert>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Create brand
          </Button>
        </div>
      </form>
    </Card>
  )
}
