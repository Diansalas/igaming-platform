import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import {
  createStaff,
  MIN_STAFF_PASSWORD_LENGTH,
  STAFF_ROLES,
  TENANT_CREATABLE_STAFF_ROLES,
  type StaffRole,
  type StaffUser,
} from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { ErrorAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { SelectField, TextInput } from '../../components/FormField'
import { describeError, type ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export const PERSON_ID_HELP =
  'Person IDs come from `deploy.sh seed-admin <email>` output (linked to person …). Two finance approvers must use two different person IDs.'

/**
 * POST /v1/admin/tenants/{tenantID}/staff - PermStaffManage. Role options
 * mirror newCreateStaffHandler: a platform-scoped caller may create every
 * role; a tenant-scoped caller (tenant_admin) only tenant_admin/support/
 * compliance. The backend remains authoritative (403 otherwise).
 *
 * There is NO staff list endpoint, so the created staff id / person_id are
 * displayed prominently for the operator to note down.
 */
export function CreateStaffForm({ tenantId }: { tenantId: string }) {
  const { claims } = useAuth()
  const roleOptions = (isPlatformAdmin(claims) ? STAFF_ROLES : TENANT_CREATABLE_STAFF_ROLES).map((r) => ({ value: r, label: r }))
  const guard = useOnceGuard()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<StaffRole>(roleOptions[0].value)
  const [personId, setPersonId] = useState('')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [created, setCreated] = useState<StaffUser | null>(null)

  const trimmedPersonId = personId.trim()
  const passwordTooShort = password.length > 0 && password.length < MIN_STAFF_PASSWORD_LENGTH
  const personIdInvalid = trimmedPersonId !== '' && !UUID_RE.test(trimmedPersonId)
  const incomplete = !email.trim() || password.length < MIN_STAFF_PASSWORD_LENGTH || personIdInvalid

  const mutation = useMutation({
    mutationFn: () =>
      createStaff(tenantId, {
        email: email.trim(),
        password,
        role,
        ...(trimmedPersonId ? { person_id: trimmedPersonId } : {}),
      }),
    onSuccess: (staff) => {
      setError(null)
      setCreated(staff)
      setEmail('')
      setPassword('')
      setPersonId('')
    },
    onError: (err) => setError(describeError(err, 'Failed to create staff user.')),
    onSettled: () => guard.release(),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setCreated(null)
    guard.run(() => mutation.mutate())
  }

  return (
    <Card title="Create staff user">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput label="Staff email" type="email" autoComplete="off" value={email} onChange={(e) => setEmail(e.target.value)} required />
          <TextInput
            label="Initial password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            help={
              passwordTooShort ? (
                <span className="text-red-600">Password must be at least {MIN_STAFF_PASSWORD_LENGTH} characters.</span>
              ) : (
                `At least ${MIN_STAFF_PASSWORD_LENGTH} characters.`
              )
            }
            required
          />
          <SelectField
            label="Role"
            options={roleOptions}
            value={role}
            onChange={(e) => setRole(e.target.value as StaffRole)}
            help={
              isPlatformAdmin(claims)
                ? undefined
                : 'finance, risk_manager, promotions_manager and bonus_operations staff can only be created by a platform administrator.'
            }
          />
          <TextInput
            label="Person ID (optional)"
            value={personId}
            onChange={(e) => setPersonId(e.target.value)}
            placeholder="00000000-0000-0000-0000-000000000000"
            help={personIdInvalid ? <span className="text-red-600">Person ID must be a UUID.</span> : PERSON_ID_HELP}
          />
        </div>
        {role === 'finance' && !trimmedPersonId && (
          <p className="text-sm text-amber-700">
            A finance user without a person ID cannot approve, reject, submit or resolve withdrawals until one is linked (see
            "Link staff to person" below).
          </p>
        )}
        <ErrorAlert error={error} />
        {created && (
          <div role="status" className="rounded-md border border-green-300 bg-green-50 p-4 text-sm text-green-900">
            <p className="font-semibold">Staff user created - note these values now (there is no staff list API).</p>
            <dl className="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-2">
              <div>
                <dt className="text-green-700">Staff ID</dt>
                <dd className="break-all font-mono text-base">{created.id}</dd>
              </div>
              <div>
                <dt className="text-green-700">Person ID</dt>
                <dd className="break-all font-mono text-base">{created.person_id ?? 'not linked'}</dd>
              </div>
              <div>
                <dt className="text-green-700">Email</dt>
                <dd>{created.email}</dd>
              </div>
              <div>
                <dt className="text-green-700">Role / status</dt>
                <dd>
                  {created.role} / {created.status}
                </dd>
              </div>
            </dl>
          </div>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Create staff user
          </Button>
        </div>
      </form>
    </Card>
  )
}
