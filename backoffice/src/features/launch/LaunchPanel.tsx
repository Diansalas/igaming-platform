import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { listLaunchHistory, listLaunchRequests, type LaunchResult, type RequestableAction } from '../../api/launch'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { getNavPermissions } from '../../auth/permissions'
import { SuccessAlert } from '../../components/Alert'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { SuspendDialog } from './DecisionDialogs'
import { HistoryTimeline } from './HistoryTimeline'
import { LaunchDisclaimer, LaunchErrorAlert, LaunchStatusBadge, LegacyBaselineBadge } from './LaunchBits'
import { RequestDetail } from './RequestDetail'
import { RequestDialog } from './RequestDialog'
import { availableActions, isLegacyBaseline, subjectTransitions } from './actions'
import { actionLabel, reasonLabel, requestStatusLabel, requestStatusTone, subjectStatusLabel } from './labels'

export interface LaunchPanelProps {
  tenantId: string
  /** Absent: the subject is the tenant itself. */
  brandId?: string
  /** The subject's current status as returned by the tenant/brand GET. */
  status: string
  subjectName: string
  /** Re-read the tenant/brand after a decision may have changed its status. */
  onSubjectChanged: () => void
}

/**
 * ADR 0112 launch panel for a tenant or brand detail page. Shows status, legacy-baseline badge,
 * the request list and detail (readiness, licensing, approvals), the offered actions, and the
 * status history. Hiding a button is a convenience; the server decides.
 */
export function LaunchPanel({ tenantId, brandId, status, subjectName, onSubjectChanged }: LaunchPanelProps) {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const platformScope = isPlatformAdmin(claims)
  const subjectKind = brandId ? 'brand' : 'tenant'
  const qc = useQueryClient()
  const [params, setParams] = useSearchParams()
  const selected = params.get('request')
  const [requestAction, setRequestAction] = useState<RequestableAction | null>(null)
  const [suspendOpen, setSuspendOpen] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)

  const historyQuery = useQuery({
    queryKey: ['launch-history', tenantId, brandId ?? null],
    queryFn: () => listLaunchHistory(tenantId, { brand_id: brandId }),
    enabled: perms.launchRead,
    retry: false,
  })
  const requestsQuery = useQuery({
    queryKey: ['launch-requests', tenantId, brandId ?? null],
    queryFn: () => listLaunchRequests(tenantId, { brand_id: brandId }),
    enabled: perms.launchRead,
    retry: false,
  })

  if (!perms.launchRead) return null

  const history = subjectTransitions(historyQuery.data?.items ?? [], subjectKind)
  const legacy = isLegacyBaseline(history)
  const requests = (requestsQuery.data?.items ?? []).filter((r) => r.subject_kind === subjectKind)
  const hasPending = requests.some((r) => r.status === 'pending' && r.action !== 'suspend')
  const actions = availableActions({ status, subjectKind, legacyBaseline: legacy, perms, platformScope, hasPending })

  function changed() {
    void qc.invalidateQueries({ queryKey: ['launch-history', tenantId] })
    void qc.invalidateQueries({ queryKey: ['launch-requests', tenantId] })
    void qc.invalidateQueries({ queryKey: ['launch-queue'] })
    onSubjectChanged()
  }

  function select(id: string | null) {
    const next = new URLSearchParams(params)
    if (id) next.set('request', id)
    else next.delete('request')
    setParams(next, { replace: true })
  }

  function filed(res: LaunchResult) {
    setRequestAction(null)
    setNotice(
      `${actionLabel(res.request.action)} request filed. It changes nothing until ${res.request.required_approvals} distinct platform approver${res.request.required_approvals === 1 ? '' : 's'} approve${res.request.required_approvals === 1 ? 's' : ''} it.`,
    )
    select(res.request.id)
    changed()
  }

  function suspended(res: LaunchResult) {
    setSuspendOpen(false)
    const n = res.superseded_request_ids?.length ?? 0
    setNotice(
      `Suspended. The status is now ${subjectStatusLabel(res.request.to_status)}.${n > 0 ? ` ${n} pending request${n === 1 ? ' was' : 's were'} superseded.` : ''}`,
    )
    changed()
  }

  return (
    <Card
      title={
        <span className="flex flex-wrap items-center gap-2" aria-label="Launch status">
          Launch authorisation <LaunchStatusBadge status={status} /> {legacy && <LegacyBaselineBadge />}
        </span>
      }
    >
      <div className="flex flex-col gap-4">
        <LaunchDisclaimer apiDisclaimer={requestsQuery.data?.disclaimer ?? historyQuery.data?.disclaimer} />
        {notice && <SuccessAlert>{notice}</SuccessAlert>}

        {(actions.request.length > 0 || actions.suspend) && (
          <div className="flex flex-wrap gap-2" aria-label="Launch actions">
            {actions.request.map((a) => (
              <Button key={a} variant={a === 'close' ? 'danger' : 'primary'} onClick={() => setRequestAction(a)}>
                {a === 'close' ? 'Request closure' : a === 'ratify' ? 'Request ratification' : `Request ${a}`}
              </Button>
            ))}
            {actions.suspend && (
              <Button variant="danger" onClick={() => setSuspendOpen(true)}>
                Suspend
              </Button>
            )}
          </div>
        )}

        <section aria-label="Launch requests" className="flex flex-col gap-2">
          <h4 className="text-sm font-semibold text-slate-800">Requests</h4>
          {requestsQuery.isLoading && <LoadingSpinner label="Loading requests..." />}
          {requestsQuery.error && <LaunchErrorAlert error={requestsQuery.error} />}
          {!requestsQuery.isLoading && !requestsQuery.error && requests.length === 0 && (
            <p className="text-sm text-slate-500">No launch requests for this {subjectKind}.</p>
          )}
          {requests.length > 0 && (
            <ul className="flex flex-col gap-1">
              {requests.map((r) => (
                <li key={r.id}>
                  <button
                    type="button"
                    onClick={() => select(selected === r.id ? null : r.id)}
                    className="flex w-full flex-wrap items-center gap-2 rounded-md border border-border px-3 py-2 text-left text-sm hover:bg-surface-alt"
                    aria-expanded={selected === r.id}
                  >
                    <span className="font-medium">{actionLabel(r.action)}</span>
                    <Badge tone={requestStatusTone(r.status)}>{requestStatusLabel(r.status)}</Badge>
                    <span className="text-xs text-slate-500">
                      {reasonLabel(r.reason_code)} - {new Date(r.created_at).toLocaleString()}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>

        {selected && <RequestDetail key={selected} tenantId={tenantId} requestId={selected} onChanged={changed} />}

        <section aria-label="Status history" className="flex flex-col gap-2">
          <h4 className="text-sm font-semibold text-slate-800">Status history</h4>
          {historyQuery.isLoading && <LoadingSpinner label="Loading history..." />}
          {historyQuery.error && <LaunchErrorAlert error={historyQuery.error} />}
          {historyQuery.data && <HistoryTimeline items={history} />}
        </section>
      </div>

      {requestAction && (
        <RequestDialog
          tenantId={tenantId}
          brandId={brandId}
          action={requestAction}
          canReadRegistries={platformScope}
          onClose={() => setRequestAction(null)}
          onDone={filed}
        />
      )}
      {suspendOpen && (
        <SuspendDialog tenantId={tenantId} brandId={brandId} subjectLabel={subjectName} onClose={() => setSuspendOpen(false)} onDone={suspended} />
      )}
    </Card>
  )
}
