import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { reviewVerification, type KycCase } from '../../api/kyc'
import { ApiError } from '../../api/types'
import { useAuth } from '../../auth/AuthContext'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Modal } from '../../components/Modal'
import { kycStatusTone } from './status'

function Field({ label, value }: { label: string; value?: string }) {
  if (!value) return null
  return (
    <div>
      <dt className="text-xs text-slate-500">{label}</dt>
      <dd className="mt-0.5 break-all text-sm text-slate-800">{value}</dd>
    </div>
  )
}

/**
 * Case detail view + the ONE existing reviewer-decision endpoint
 * (POST /v1/admin/kyc/verifications/{id}/review) - no new KYC decision
 * endpoint is invented here, per the Stage 5 directive. The review form
 * is only rendered for the `compliance` role (the sole holder of
 * PermVerificationReview server-side) - a UI convenience; if a
 * differently-permissioned caller ever reached this action some other
 * way, the server would still reject it with a 403.
 */
export function KycCaseDetail({ kycCase, onClose }: { kycCase: KycCase; onClose: () => void }) {
  const { claims } = useAuth()
  const queryClient = useQueryClient()
  const canReview = claims?.role === 'compliance'
  const [reason, setReason] = useState('')
  const [errorMessage, setErrorMessage] = useState<string | null>(null)

  const mutation = useMutation({
    mutationFn: (status: 'approved' | 'rejected' | 'review_required') =>
      reviewVerification(kycCase.id, { status, reason }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['kyc-cases'] })
      onClose()
    },
    onError: (err) => {
      setErrorMessage(err instanceof ApiError ? err.message : 'Failed to record the review decision.')
    },
  })

  return (
    <Modal title="KYC case detail" onClose={onClose}>
      <dl className="grid grid-cols-2 gap-3">
        <Field label="Player account" value={kycCase.player_account_id} />
        <Field label="Brand" value={kycCase.brand_id} />
        <div>
          <dt className="text-xs text-slate-500">Status</dt>
          <dd className="mt-0.5">
            <Badge tone={kycStatusTone(kycCase.status)}>{kycCase.status}</Badge>
          </dd>
        </div>
        <Field label="Provider" value={kycCase.provider_id} />
        <Field label="Provider reference" value={kycCase.provider_reference} />
        <Field label="Reason" value={kycCase.reason} />
        <Field label="Submitted at" value={kycCase.submitted_at} />
        <Field label="Reviewed at" value={kycCase.reviewed_at} />
        <Field label="Reviewed by" value={kycCase.reviewed_by} />
        <Field label="Expires at" value={kycCase.expires_at} />
        <Field label="Verified residence on file" value={kycCase.has_verified_residence ? 'Yes' : 'No'} />
      </dl>

      {canReview && (
        <div className="mt-5 border-t border-border pt-4">
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Reason</span>
            <textarea
              className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              rows={2}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          </label>
          {errorMessage && <p className="mt-2 text-sm text-red-600">{errorMessage}</p>}
          <div className="mt-3 flex justify-end gap-2">
            <Button
              variant="danger"
              isLoading={mutation.isPending}
              onClick={() => mutation.mutate('rejected')}
            >
              Reject
            </Button>
            <Button
              variant="secondary"
              isLoading={mutation.isPending}
              onClick={() => mutation.mutate('review_required')}
            >
              Send back for review
            </Button>
            <Button variant="primary" isLoading={mutation.isPending} onClick={() => mutation.mutate('approved')}>
              Approve
            </Button>
          </div>
        </div>
      )}
    </Modal>
  )
}
