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
import { CHANGE_REQUEST_STATUS_OPTIONS, changeRequestStatusTone } from './status'

type PendingAction = { request: ChangeRequest; decision: 'approve' | 'reject' }

export function ChangeRequestQueuePage() {
  const [status, setStatus] = useState<ChangeRequestStatus>('pending')
  const [pendingAction, setPendingAction] = useState<PendingAction | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()
  const queryClient = useQueryClient()

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
  })

  const columns: Column<ChangeRequest>[] = [
    { key: 'operation', header: 'Operation', render: (r) => r.operation },
    { key: 'target', header: 'Target', render: (r) => <span className="font-mono text-xs">{r.target_type}/{r.target_id}</span> },
    { key: 'amount', header: 'Amount', render: (r) => (r.amount_at_request ? `${r.amount_at_request} ${r.asset_code ?? ''}` : '—') },
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
          description={`This will ${pendingAction.decision} the ${pendingAction.request.operation} request for ${pendingAction.request.target_type}/${pendingAction.request.target_id}. The server independently enforces four-eyes/requester-approver separation regardless of this confirmation.`}
          variant={pendingAction.decision === 'reject' ? 'danger' : 'primary'}
          confirmLabel={pendingAction.decision === 'approve' ? 'Approve' : 'Reject'}
          requireReason={pendingAction.decision === 'reject'}
          reasonLabel="Reason code"
          isSubmitting={mutation.isPending}
          errorMessage={actionError}
          onConfirm={(reason) =>
            mutation.mutate({ ...pendingAction, reasonCode: reason || undefined })
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
