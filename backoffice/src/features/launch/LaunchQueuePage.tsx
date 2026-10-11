import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { listLaunchQueue, type LaunchRequest } from '../../api/launch'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { LaunchDisclaimer, LaunchErrorAlert, actorLabel } from './LaunchBits'
import { REQUEST_STATUS_FILTER_OPTIONS, actionLabel, reasonLabel, requestStatusLabel, requestStatusTone } from './labels'

/**
 * Platform-wide launch authorisation queue. Route-gated by the `launchQueue` flag (platform roles)
 * and, authoritatively, by the server (a tenant caller is 403). Opening a row goes to the subject's
 * detail page where the approve/reject dialogs live.
 */
export function LaunchQueuePage() {
  const navigate = useNavigate()
  const [status, setStatus] = useState('pending')
  const query = useQuery({
    queryKey: ['launch-queue', status],
    queryFn: () => listLaunchQueue({ status: status || undefined }),
    retry: false,
  })
  const items = query.data?.items ?? []

  function open(r: LaunchRequest) {
    const base = r.brand_id ? `/tenants/${r.tenant_id}/brands/${r.brand_id}` : `/tenants/${r.tenant_id}`
    navigate(`${base}?request=${r.id}`)
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Launch queue" description="Launch authorisation requests across all tenants, newest first (at most 100)." />
      <LaunchDisclaimer apiDisclaimer={query.data?.disclaimer} />
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
                <th className="px-3 py-2 font-medium">Needs</th>
                <th className="px-3 py-2 font-medium">Filed by</th>
                <th className="px-3 py-2 font-medium">Filed</th>
                <th className="px-3 py-2 font-medium">Expires</th>
              </tr>
            </thead>
            <tbody>
              {items.map((r) => (
                <tr key={r.id} className="cursor-pointer border-t border-border hover:bg-surface-alt" onClick={() => open(r)}>
                  <td className="px-3 py-2 font-mono text-xs">{r.tenant_id.slice(0, 8)}</td>
                  <td className="px-3 py-2">{r.subject_kind === 'brand' ? `Brand ${(r.brand_id ?? '').slice(0, 8)}` : 'Tenant'}</td>
                  <td className="px-3 py-2">
                    <button type="button" className="text-brand-700 hover:underline" onClick={() => open(r)}>
                      {actionLabel(r.action)}
                    </button>{' '}
                    <span className="text-xs text-slate-500">({reasonLabel(r.reason_code)})</span>
                  </td>
                  <td className="px-3 py-2">
                    <Badge tone={requestStatusTone(r.status)}>{requestStatusLabel(r.status)}</Badge>
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
    </div>
  )
}
