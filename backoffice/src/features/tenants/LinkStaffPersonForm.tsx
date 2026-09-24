import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { linkStaffPerson } from '../../api/tenants'
import { ErrorAlert, SuccessAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { TextInput } from '../../components/FormField'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'
import { PERSON_ID_HELP } from './CreateStaffForm'

/**
 * POST /v1/admin/tenants/{tenantID}/staff/{staffID}/person-link - PermStaffManage.
 * Only sets a link where none exists; the backend refuses (409) to change an existing one.
 */
export function LinkStaffPersonForm({ tenantId }: { tenantId: string }) {
  const guard = useOnceGuard()
  const [staffId, setStaffId] = useState('')
  const [personId, setPersonId] = useState('')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [linked, setLinked] = useState<{ staffId: string; personId: string } | null>(null)

  const mutation = useMutation({
    mutationFn: (vars: { staffId: string; personId: string }) => linkStaffPerson(tenantId, vars.staffId, vars.personId),
    onSuccess: (_data, vars) => {
      setError(null)
      setLinked(vars)
      setStaffId('')
      setPersonId('')
    },
    onError: (err) => setError(describeError(err, 'Failed to link staff to person.')),
    onSettled: () => guard.release(),
  })

  const incomplete = !staffId.trim() || !personId.trim()

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setLinked(null)
    const vars = { staffId: staffId.trim(), personId: personId.trim() }
    guard.run(() => mutation.mutate(vars))
  }

  return (
    <Card title="Link staff to person">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <p className="text-sm text-slate-500">
          For a staff account created without a person ID. An existing link can never be changed.
        </p>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput label="Staff ID to link" value={staffId} onChange={(e) => setStaffId(e.target.value)} required />
          <TextInput label="Person ID to link" value={personId} onChange={(e) => setPersonId(e.target.value)} help={PERSON_ID_HELP} required />
        </div>
        <ErrorAlert error={error} />
        {linked && (
          <SuccessAlert>
            Staff <span className="font-mono">{linked.staffId}</span> linked to person <span className="font-mono">{linked.personId}</span>.
          </SuccessAlert>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Link person
          </Button>
        </div>
      </form>
    </Card>
  )
}
