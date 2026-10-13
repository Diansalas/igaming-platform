import { useQuery } from '@tanstack/react-query'
import { getMyMfaStatus } from '../../api/auth'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { InfoNote } from '../../components/Alert'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { ApiError } from '../../api/types'
import { UNAVAILABLE } from '../../auth/staffAuthErrors'

/** Warn at this many remaining recovery codes or fewer (ADR 0119 s15.4, contract 1.5). */
export const RECOVERY_WARN_AT = 3

function formatWhen(iso: string | null): string {
  if (!iso) return 'Not available'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? 'Not available' : d.toISOString().replace('T', ' ').slice(0, 16) + ' UTC'
}

/**
 * "My security" (ADR 0119 S7a): read-only MFA status from GET /v1/staff/me/mfa. Display only - the server
 * decides what any staff member may do. Recovery-code regeneration needs step-up (S4b/S7b) and is NOT
 * IMPLEMENTED here: there is no button, no endpoint call and no placeholder action.
 */
export function SecurityPage() {
  const q = useQuery({ queryKey: ['staff-mfa-status'], queryFn: getMyMfaStatus, retry: false })

  return (
    <div>
      <PageHeader title="My security" description="Your authenticator and recovery codes." />
      {q.isPending && <LoadingSpinner label="Loading your security status..." />}
      {q.isError && (
        <p role="alert" className="text-sm text-red-600">
          {q.error instanceof ApiError && q.error.code === 'service_unavailable'
            ? UNAVAILABLE
            : 'Could not load your security status. Try again.'}
        </p>
      )}
      {q.data && (
        <div className="flex max-w-xl flex-col gap-4">
          <Card title="Authenticator">
            <dl className="grid grid-cols-[10rem_1fr] gap-y-2 text-sm">
              <dt className="text-slate-500">Status</dt>
              <dd>
                <Badge tone={q.data.enrolled ? 'success' : 'danger'}>{q.data.enrolled ? 'Enrolled' : 'Not enrolled'}</Badge>
              </dd>
              <dt className="text-slate-500">Type</dt>
              <dd>{q.data.factor_type === 'totp' ? 'Authenticator app (TOTP)' : 'None'}</dd>
              <dt className="text-slate-500">Activated</dt>
              <dd>{formatWhen(q.data.activated_at)}</dd>
              <dt className="text-slate-500">Lock</dt>
              <dd>
                {q.data.hard_locked
                  ? 'Locked. Ask your administrator to reset your authenticator.'
                  : q.data.locked_until
                    ? `Temporarily locked until ${formatWhen(q.data.locked_until)}`
                    : 'Not locked'}
              </dd>
            </dl>
          </Card>
          <Card title="Recovery codes">
            <p className="text-sm">
              <span className="font-medium">{q.data.recovery_codes_remaining}</span> unused recovery codes remaining.
            </p>
            {q.data.enrolled && q.data.recovery_codes_remaining <= RECOVERY_WARN_AT && (
              <p role="alert" className="mt-2 text-sm text-amber-700">
                {q.data.recovery_codes_remaining === 0
                  ? 'You have no recovery codes left. Ask your administrator how to get new ones.'
                  : 'You are running low on recovery codes. Ask your administrator how to get new ones.'}
              </p>
            )}
            <div className="mt-3">
              <InfoNote>
                Regenerating recovery codes is not available yet (NOT IMPLEMENTED: it needs step-up
                authentication, a later slice).
              </InfoNote>
            </div>
          </Card>
        </div>
      )}
    </div>
  )
}
