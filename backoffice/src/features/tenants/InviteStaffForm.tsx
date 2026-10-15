import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import {
  inviteStaff,
  PERSON_REQUIRED_STAFF_ROLES,
  STAFF_ROLES,
  TENANT_GRANTABLE_STAFF_ROLES,
  type StaffLifecycleResult,
  type StaffRole,
} from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { ErrorAlert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { SelectField, TextInput } from '../../components/FormField'
import type { ErrorDetails } from '../../lib/apiErrorMessage'
import { useOnceGuard } from '../../lib/useOnceGuard'
import { PERSON_ID_HELP, UUID_RE } from './personHelp'
import { describeInviteError } from './staffErrors'

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

/**
 * POST /v1/admin/tenants/{tenantID}/staff/invitations - staff:manage, step-up critical (ADR 0119 S5/S6).
 *
 * Staff are created by INVITATION: no password is chosen or seen here. The server emails a single-use link to the
 * invitee, who sets their own password and enrols an authenticator. The response never carries a token.
 *
 * Role options mirror the contract's role authority (platform admin: every tenant role; tenant admin: tenant_admin,
 * support, compliance). That is a convenience only: the server answers `403 role_not_grantable` otherwise and this
 * form shows that refusal cleanly. The step-up prompt is raised by the shared apiFetch interceptor (StepUpProvider);
 * this form adds nothing to it and never retries on its own.
 */
export function InviteStaffForm({ tenantId }: { tenantId: string }) {
  const { claims } = useAuth()
  const queryClient = useQueryClient()
  const roleOptions = (isPlatformAdmin(claims) ? STAFF_ROLES : TENANT_GRANTABLE_STAFF_ROLES).map((r) => ({ value: r, label: r }))
  const guard = useOnceGuard()
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<StaffRole>(roleOptions[0].value)
  const [personId, setPersonId] = useState('')
  const [error, setError] = useState<ErrorDetails | null>(null)
  const [invited, setInvited] = useState<StaffLifecycleResult | null>(null)

  const trimmedEmail = email.trim()
  const trimmedPersonId = personId.trim()
  const personRequired = PERSON_REQUIRED_STAFF_ROLES.includes(role)
  const emailInvalid = trimmedEmail !== '' && !EMAIL_RE.test(trimmedEmail)
  const personIdInvalid = trimmedPersonId !== '' && !UUID_RE.test(trimmedPersonId)
  const incomplete = !EMAIL_RE.test(trimmedEmail) || personIdInvalid || (personRequired && trimmedPersonId === '')

  const mutation = useMutation({
    mutationFn: () =>
      inviteStaff(tenantId, {
        email: trimmedEmail,
        role,
        ...(trimmedPersonId ? { person_id: trimmedPersonId } : {}),
      }),
    onSuccess: (result) => {
      setError(null)
      setInvited(result)
      setEmail('')
      setPersonId('')
      void queryClient.invalidateQueries({ queryKey: ['tenant-staff', tenantId] })
    },
    onError: (err) => setError(describeInviteError(err)),
    onSettled: () => guard.release(),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (incomplete) return
    setInvited(null)
    setError(null)
    guard.run(() => mutation.mutate())
  }

  return (
    <Card title="Invite staff user">
      <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate aria-label="Invite staff user">
        <p className="text-sm text-slate-500">
          The invitee receives a single-use link by email (valid 72 hours), sets their own password and enrols an authenticator
          app. No password is chosen or shown here. You will be asked to confirm with your authenticator code.
        </p>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput
            label="Staff email"
            type="email"
            autoComplete="off"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            aria-invalid={emailInvalid || undefined}
            help={emailInvalid ? <span className="text-red-600">Enter a valid email address.</span> : undefined}
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
                : 'finance, risk_manager, promotions_manager and bonus_operations staff can only be invited by a platform administrator.'
            }
          />
          <TextInput
            label={personRequired ? 'Person ID' : 'Person ID (optional)'}
            value={personId}
            onChange={(e) => setPersonId(e.target.value)}
            placeholder="00000000-0000-0000-0000-000000000000"
            aria-invalid={personIdInvalid || undefined}
            required={personRequired}
            help={
              personIdInvalid ? (
                <span className="text-red-600">Person ID must be a UUID.</span>
              ) : personRequired ? (
                `A linked Person is required for ${role}. ${PERSON_ID_HELP}`
              ) : (
                PERSON_ID_HELP
              )
            }
          />
        </div>
        <ErrorAlert error={error} />
        {invited && (
          <div role="status" className="rounded-md border border-green-300 bg-green-50 p-4 text-sm text-green-900">
            <p className="font-semibold">Invitation created.</p>
            <dl className="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-2">
              <div>
                <dt className="text-green-700">Email</dt>
                <dd>{invited.staff.email}</dd>
              </div>
              <div>
                <dt className="text-green-700">Role / status</dt>
                <dd>
                  {invited.staff.role} / {invited.staff.status}
                </dd>
              </div>
            </dl>
            {invited.email_delivery === 'failed' ? (
              <p className="mt-2 text-amber-800">
                The email could not be handed to the mail provider, so the invitee has not received the link. The invitation
                exists; re-sending is not available in this console yet.
              </p>
            ) : (
              <p className="mt-2">The invitation email was handed to the mail provider.</p>
            )}
          </div>
        )}
        <div>
          <Button type="submit" disabled={incomplete} isLoading={mutation.isPending}>
            Send invitation
          </Button>
        </div>
      </form>
    </Card>
  )
}
