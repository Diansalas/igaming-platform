import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getMyRgStatus, listMyKycVerifications, startKycVerification } from '../../api/compliance'
import { ApiError } from '../../api/types'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'

/**
 * Shows the player's identity-verification (KYC) cases and responsible-
 * gaming restrictions exactly as the server reports them, and offers the
 * existing self-service "start identity verification" action. It makes no
 * regulatory decision: review happens in the Back Office, and the KYC tier
 * shown on the profile only changes when the server changes it.
 */
export function ComplianceStatusCard({ kycTier }: { kycTier: number }) {
  const queryClient = useQueryClient()
  const kycQuery = useQuery({ queryKey: ['kyc-verifications'], queryFn: listMyKycVerifications })
  const rgQuery = useQuery({ queryKey: ['rg-status'], queryFn: getMyRgStatus })

  const startMutation = useMutation({
    mutationFn: startKycVerification,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['kyc-verifications'] })
    },
  })

  const latest = kycQuery.data?.[0]
  const activeRestrictions = (rgQuery.data ?? []).filter((r) => r.active)

  return (
    <Card title="Verification & responsible gaming">
      <div className="grid gap-6 text-sm sm:grid-cols-2">
        <section className="flex flex-col gap-2">
          <h4 className="font-medium text-slate-800">Identity verification (KYC)</h4>
          <p className="text-slate-600">KYC tier: {kycTier}</p>
          {kycQuery.isLoading && <LoadingSpinner label="Loading verification status..." />}
          {kycQuery.error && <ErrorState error={kycQuery.error} onRetry={() => void kycQuery.refetch()} />}
          {kycQuery.data && kycQuery.data.length === 0 && <p className="text-slate-600">No verification started.</p>}
          {latest && (
            <p className="text-slate-600">
              Latest case: <Badge tone={latest.status === 'approved' ? 'success' : 'warning'}>{latest.status}</Badge>
              {latest.reason && <span className="ml-2 text-xs text-slate-500">{latest.reason}</span>}
            </p>
          )}
          {kycQuery.data && (
            <div>
              <Button variant="secondary" isLoading={startMutation.isPending} onClick={() => startMutation.mutate()}>
                Start identity verification
              </Button>
            </div>
          )}
          {startMutation.isSuccess && (
            <p role="status" className="text-emerald-700">
              Verification case opened. It will be reviewed by the compliance team.
            </p>
          )}
          {startMutation.error && (
            <p role="alert" className="text-red-700">
              {startMutation.error instanceof ApiError ? startMutation.error.message : 'Could not start verification.'}
            </p>
          )}
        </section>

        <section className="flex flex-col gap-2">
          <h4 className="font-medium text-slate-800">Responsible gaming</h4>
          {rgQuery.isLoading && <LoadingSpinner label="Loading restrictions..." />}
          {rgQuery.error && <ErrorState error={rgQuery.error} onRetry={() => void rgQuery.refetch()} />}
          {rgQuery.data && activeRestrictions.length === 0 && (
            <p className="text-slate-600">No active restrictions on your account.</p>
          )}
          {activeRestrictions.length > 0 && (
            <ul className="flex flex-col gap-1">
              {activeRestrictions.map((r) => (
                <li key={r.id} className="text-slate-700">
                  <Badge tone="danger">{r.restriction_type}</Badge> <span className="text-xs">({r.scope})</span>{' '}
                  <span className="text-xs text-slate-500">
                    {r.indefinite ? 'indefinite' : r.ends_at ? `until ${new Date(r.ends_at).toLocaleString()}` : ''}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </Card>
  )
}
