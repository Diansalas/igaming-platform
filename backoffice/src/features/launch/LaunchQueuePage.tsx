import { useInfiniteQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { listLaunchQueue, type CannotApproveReason, type LaunchRequest } from '../../api/launch'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { LaunchDisclaimer, LaunchErrorAlert, actorLabel } from './LaunchBits'
import { approvalControls, cannotApproveText } from './approvability'
import { REQUEST_STATUS_FILTER_OPTIONS, actionLabel, reasonLabel, requestStatusLabel, requestStatusTone } from './labels'

/**
 * Platform-wide launch authorisation queue. Route-gated by the `launchQueue` flag (platform roles)
 * and, authoritatively, by the server (a tenant caller is 403). Opening a row goes to the subject's
 * detail page where the approve/reject dialogs live.
 */
const PAGE_SIZE = 50

export function LaunchQueuePage() {
  const navigate = useNavigate()
  const [status, setStatus] = useState('pending')
  // Keyset paging (ADR 0112 slice 6 (c)): 50 per page, the server's next_cursor is the only position.
  const query = useInfiniteQuery({
    queryKey: ['launch-queue', status],
    queryFn: ({ pageParam }) => listLaunchQueue({ status: status || undefined, limit: PAGE_SIZE, cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    retry: false,
  })
  const pages = query.data?.pages ?? []
  const items = pages.flatMap((p) => p.items)

  function open(r: LaunchRequest) {
    const t = encodeURIComponent(r.tenant_id)
    const base = r.brand_id ? `/tenants/${t}/brands/${encodeURIComponent(r.brand_id)}` : `/tenants/${t}`
    navigate(`${base}?request=${encodeURIComponent(r.id)}`)
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Launch queue" description="Launch authorisation requests across all tenants, newest first, 50 at a time." />
      <LaunchDisclaimer apiDisclaimer={pages[0]?.disclaimer} />
      <div className="flex gap-2">
        <Select
          aria-label="Request status"
          placeholder="All statuses"
          options={REQUEST_STATUS_FILTER_OPTIONS}
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        />
      </div>
      {query.isLoading && <LoadingSpinner label="Loading queue..." />}
      {query.error && <LaunchErrorAlert error={query.error} />}
      {query.data && items.length === 0 && <p className="text-sm text-slate-500">No requests match this filter.</p>}
      {items.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
          <table className="w-full text-left text-sm">
            <thead className="bg-surface-alt text-xs text-slate-500">
              <tr>
                <th className="px-3 py-2 font-medium">Tenant</th>
                <th className="px-3 py-2 font-medium">Subject</th>
                <th className="px-3 py-2 font-medium">Action</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 font-medium">You</th>
                <th className="px-3 py-2 font-medium">Needs</th>
                <th className="px-3 py-2 font-medium">Filed by</th>
                <th className="px-3 py-2 font-medium">Filed</th>
                <th className="px-3 py-2 font-medium">Expires</th>
              </tr>
            </thead>
            <tbody>
              {items.map((r) => (
                <tr key={r.id} className="cursor-pointer border-t border-border hover:bg-surface-alt" onClick={() => open(r)}>
                  <td className="px-3 py-2">{r.tenant_name ?? <span className="font-mono text-xs">{r.tenant_id.slice(0, 8)}</span>}</td>
                  <td className="px-3 py-2">{subjectLabel(r)}</td>
                  <td className="px-3 py-2">
                    <button type="button" className="text-brand-700 hover:underline" onClick={() => open(r)}>
                      {actionLabel(r.action)}
                    </button>{' '}
                    <span className="text-xs text-slate-500">({reasonLabel(r.reason_code)})</span>
                  </td>
                  <td className="px-3 py-2">
                    <Badge tone={requestStatusTone(r.status)}>{requestStatusLabel(r.status)}</Badge>
                  </td>
                  <td className="px-3 py-2">
                    <YouCell r={r} />
                  </td>
                  <td className="px-3 py-2">{r.required_approvals} approval{r.required_approvals === 1 ? '' : 's'}</td>
                  <td className="px-3 py-2">{actorLabel(r.requested_by, r.requested_by_scope)}</td>
                  <td className="px-3 py-2">{new Date(r.created_at).toLocaleString()}</td>
                  <td className="px-3 py-2">{new Date(r.expires_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {query.hasNextPage && (
        <div>
          <Button variant="secondary" isLoading={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
            Load more
          </Button>
        </div>
      )}
    </div>
  )
}

function subjectLabel(r: LaunchRequest): string {
  if (r.subject_kind !== 'brand') return 'Tenant'
  return r.brand_name ? `Brand: ${r.brand_name}` : `Brand ${(r.brand_id ?? '').slice(0, 8)}`
}

/** The server's per-caller answer (can_approve / cannot_approve_reason), mirrored; never computed here. */
function YouCell({ r }: { r: LaunchRequest }) {
  if (r.can_approve === undefined) return <span className="text-xs text-slate-400">-</span>
  const c = approvalControls(r, true)
  if (c.approve) return <Badge tone="success">Can approve</Badge>
  return (
    <span title={cannotApproveText(r.cannot_approve_reason)}>
      <Badge tone="neutral">{(r.cannot_approve_reason && SHORT[r.cannot_approve_reason]) || 'Cannot approve'}</Badge>
    </span>
  )
}

const SHORT: Record<CannotApproveReason, string> = {
  not_platform_approver: 'Not an approver',
  not_pending: 'Not pending',
  expired: 'Expired',
  self_approval: 'You filed it',
  same_person: 'Your other account',
  no_person: 'No person link',
  already_decided: 'Already decided',
  prior_reject_requires_change_note: 'You rejected a variant',
}
