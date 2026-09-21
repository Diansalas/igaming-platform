import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { decideChangeRequest, listChangeRequests, type ChangeRequest, type ChangeRequestStatus } from '../../api/bonus'
import { ApiError } from '../../api/types'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { useAuth } from '../../auth/AuthContext'
import { formatUnscaledAmount } from '../../lib/money'
import { useOnceGuard } from '../../lib/useOnceGuard'
import { CHANGE_REQUEST_STATUS_OPTIONS, changeRequestStatusTone } from './status'

type PendingAction = { request: ChangeRequest; decision: 'approve' | 'reject' }

export function ChangeRequestQueuePage() {
  const { claims } = useAuth()
  const [status, setStatus] = useState<ChangeRequestStatus>('pending')
  const [pendingAction, setPendingAction] = useState<PendingAction | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()
  const queryClient = useQueryClient()
  // Four-eyes bonus economic-operation approval/rejection - same
  // double-submit risk as WithdrawalDetailPage (see useOnceGuard's doc
  // comment), so it gets the same synchronous once-guard on top of
  // ConfirmDialog's own isSubmitting-disables-the-button behavior.
  const decisionGuard = useOnceGuard()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['bonus-change-requests', { status, limit, offset }],
    queryFn: () => listChangeRequests({ status, limit, offset }),
  })

  const mutation = useMutation({
    mutationFn: ({ request, decision, reasonCode }: PendingAction & { reasonCode?: string }) =>
      decideChangeRequest(request.id, decision, reasonCode),
    onSuccess: () => {
      setPendingAction(null)
      setActionError(null)
      void queryClient.invalidateQueries({ queryKey: ['bonus-change-requests'] })
    },
    onError: (err) => {
      setActionError(err instanceof ApiError ? err.message : 'Failed to record this decision.')
    },
    onSettled: () => decisionGuard.release(),
  })

  const columns: Column<ChangeRequest>[] = [
    { key: 'operation', header: 'Operation', render: (r) => r.operation },
    { key: 'target', header: 'Target', render: (r) => <span className="font-mono text-xs">{r.target_type}/{r.target_id}</span> },
    {
      key: 'amount',
      header: 'Amount',
      // amount_at_request is a decimal-string minor-units value with no
      // decimal_exponent in this API response (unlike WithdrawalAdmin) -
      // see formatUnscaledAmount's doc comment. Never displayed as a
      // bare number, which would silently misrepresent the real amount
      // by whatever power of ten the asset's exponent turns out to be.
      render: (r) => (r.amount_at_request ? formatUnscaledAmount(r.amount_at_request, r.asset_code ?? '') : '—'),
    },
    { key: 'state', header: 'State', render: (r) => <Badge tone={changeRequestStatusTone(r.state)}>{r.state}</Badge> },
    { key: 'requested_at', header: 'Requested', render: (r) => r.requested_at ?? '—' },
    {
      key: 'actions',
      header: '',
      render: (r) =>
        r.state === 'pending' ? (
          <div className="flex gap-2">
            <Button
              variant="secondary"
              onClick={(e) => {
                e.stopPropagation()
                setActionError(null)
                setPendingAction({ request: r, decision: 'approve' })
              }}
            >
              Approve
            </Button>
            <Button
              variant="danger"
              onClick={(e) => {
                e.stopPropagation()
                setActionError(null)
                setPendingAction({ request: r, decision: 'reject' })
              }}
            >
              Reject
            </Button>
          </div>
        ) : null,
    },
  ]

  return (
    <div>
      <PageHeader title="Bonus change-request approvals" description="Four-eyes approval queue for bonus economic operations." />
      <div className="mb-4 flex flex-wrap gap-2">
        <Select
          options={CHANGE_REQUEST_STATUS_OPTIONS}
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as ChangeRequestStatus)
            resetToFirstPage()
          }}
        />
      </div>
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(r) => r.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No change requests match this filter."
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />

      {pendingAction && (
        <ConfirmDialog
          title={pendingAction.decision === 'approve' ? 'Approve change request' : 'Reject change request'}
          description={
            <div className="flex flex-col gap-2">
              <p>
                {`This will ${pendingAction.decision} the ${pendingAction.request.operation} request for ${pendingAction.request.target_type}/${pendingAction.request.target_id}. The server independently enforces four-eyes/requester-approver separation regardless of this confirmation.`}
              </p>
              {/* Client-side-only heads-up, not an enforcement mechanism: the
                  server (requester_principal_id vs. approver, per the
                  withdrawal_approvals precedent) is the only authoritative
                  check and will reject this exact same request if the
                  caller turns out not to be a distinct, eligible approver -
                  this just saves a confusing round trip when it's obvious
                  up front from data already on screen. */}
              {claims?.sub && pendingAction.request.requested_by_principal_id === claims.sub && (
                <p className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800">
                  You submitted this change request. Four-eyes rules require a different staff member to decide it -
                  the server will reject this if you proceed.
                </p>
              )}
            </div>
          }
          variant={pendingAction.decision === 'reject' ? 'danger' : 'primary'}
          confirmLabel={pendingAction.decision === 'approve' ? 'Approve' : 'Reject'}
          requireReason={pendingAction.decision === 'reject'}
          reasonLabel="Reason code"
          isSubmitting={mutation.isPending}
          errorMessage={actionError}
          onConfirm={(reason) =>
            decisionGuard.run(() => mutation.mutate({ ...pendingAction, reasonCode: reason || undefined }))
          }
          onCancel={() => {
            setPendingAction(null)
            setActionError(null)
          }}
        />
      )}
    </div>
  )
}
