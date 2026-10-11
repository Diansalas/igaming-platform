import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { getLaunchRequest, type LaunchRequest } from '../../api/launch'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { SuccessAlert } from '../../components/Alert'
import { ApproveDialog, CancelDialog, RejectDialog } from './DecisionDialogs'
import { approvalControls } from './approvability'
import { LaunchDisclaimer, LaunchErrorAlert, ReadinessChecklist, actorLabel } from './LaunchBits'
import {
  actionLabel,
  licensingModelLabel,
  licensingStatusLabel,
  reasonLabel,
  requestStatusLabel,
  requestStatusTone,
  subjectStatusLabel,
} from './labels'

function fmt(ts: string | undefined): string {
  return ts ? new Date(ts).toLocaleString() : '-'
}

/** Recorded licensing / operator / jurisdiction content of a request (display only). */
export function RequestContent({ request }: { request: LaunchRequest }) {
  const hasLicensing = !!request.licensing_model
  return (
    <div className="flex flex-col gap-3">
      {hasLicensing && (
        <section aria-label="Licensing" className="rounded-md border border-border p-3">
          <h4 className="mb-2 text-sm font-semibold text-slate-800">Licensing (recorded, never judged)</h4>
          <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-2">
            <Field label="Licensing model" value={licensingModelLabel(request.licensing_model)} />
            <Field label="Licensing status" value={licensingStatusLabel(request.licensing_status)} />
            {request.licence_id && <Field label="Licence id" value={request.licence_id} mono />}
            {request.determination_reference && <Field label="Determination reference" value={request.determination_reference} />}
            {request.determination_by_role && <Field label="Determination by (function)" value={request.determination_by_role} />}
            {request.responsible_operator_name && <Field label="Responsible operator" value={request.responsible_operator_name} />}
            {request.responsible_operator_registration && <Field label="Operator registration" value={request.responsible_operator_registration} />}
          </dl>
          <div className="mt-2 text-sm">
            <span className="text-slate-500">Jurisdictions: </span>
            {request.jurisdiction_ids.length === 0 ? (
              '-'
            ) : (
              <span className="font-mono text-xs">{request.jurisdiction_ids.join(', ')}</span>
            )}
          </div>
        </section>
      )}
      {request.conditions_note && (
        <section aria-label="Conditions note" className="rounded-md border border-border p-3 text-sm">
          <h4 className="mb-1 font-semibold text-slate-800">Conditions / notes</h4>
          <p className="whitespace-pre-wrap text-slate-700">{request.conditions_note}</p>
          <p className="mt-1 text-xs text-slate-500">A recorded note. Nothing in it is machine-enforced.</p>
        </section>
      )}
    </div>
  )
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-slate-500">{label}</dt>
      <dd className={`mt-0.5 break-all text-slate-900 ${mono ? 'font-mono text-xs' : ''}`}>{value}</dd>
    </div>
  )
}

export interface RequestDetailProps {
  tenantId: string
  requestId: string
  /** Called after any change so the caller can refresh subject status, lists and history. */
  onChanged: () => void
}

/**
 * One request with its approvals, prior rejects within the request window, and the actions the
 * role may attempt. Every action is re-checked by the server; a refusal is shown, never assumed.
 */
export function RequestDetail({ tenantId, requestId, onChanged }: RequestDetailProps) {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<'approve' | 'reject' | 'cancel' | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const query = useQuery({
    queryKey: ['launch-request', tenantId, requestId],
    queryFn: () => getLaunchRequest(tenantId, requestId),
    retry: false,
  })

  function refresh() {
    void qc.invalidateQueries({ queryKey: ['launch-request', tenantId, requestId] })
    onChanged()
  }

  if (query.isLoading) return <LoadingSpinner label="Loading request..." />
  if (query.error) return <LaunchErrorAlert error={query.error} />
  const data = query.data
  if (!data) return null
  const r = data.request
  const readiness = data.readiness
  const isPending = r.status === 'pending'
  const isRequester = !!claims && !!r.requested_by && r.requested_by === claims.sub
  const controls = approvalControls(r, perms.launchApprove)
  const approvals = r.approvals ?? []
  const approveCount = approvals.filter((a) => a.decision === 'approve').length
  const priorRejects = r.prior_rejects ?? []

  return (
    <div className="flex flex-col gap-4" aria-label="Launch request detail">
      <LaunchDisclaimer apiDisclaimer={data.disclaimer} />
      {notice && <SuccessAlert>{notice}</SuccessAlert>}
      <Card
        title={
          <span className="flex flex-wrap items-center gap-2">
            {actionLabel(r.action)} request <Badge tone={requestStatusTone(r.status)}>{requestStatusLabel(r.status)}</Badge>
          </span>
        }
      >
        <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-3">
          <Field label="Status change" value={`${subjectStatusLabel(r.from_status)} to ${subjectStatusLabel(r.to_status)}`} />
          <Field label="Reason" value={reasonLabel(r.reason_code)} />
          <Field label="Filed by" value={`${actorLabel(r.requested_by, r.requested_by_scope)} (${r.requested_by_scope})`} />
          <Field label="Approvals" value={`${approveCount} of ${r.required_approvals} required`} />
          <Field label="Filed" value={fmt(r.created_at)} />
          <Field label="Expires" value={fmt(r.expires_at)} />
          {r.decided_at && <Field label="Decided" value={fmt(r.decided_at)} />}
          {r.refusal_code && <Field label="Execution refusal" value={r.refusal_code.replace(/_/g, ' ')} />}
        </dl>
        {r.status === 'superseded' && (
          <p className="mt-3 text-sm text-amber-800">
            This request was superseded by a suspension of the same subject. It is not a silent disappearance: see the status history.
          </p>
        )}
        <div className="mt-3">
          <RequestContent request={r} />
        </div>
      </Card>

      {priorRejects.length > 0 && (
        <Card title="Prior rejects within the request window">
          <ul className="flex flex-col gap-1 text-sm" aria-label="Prior rejects">
            {priorRejects.map((p) => (
              <li key={p.request_id}>
                Rejected {fmt(p.decided_at)} ({reasonLabel(p.reason_code)}){' '}
                <Badge tone={p.same_payload ? 'danger' : 'warning'}>{p.same_payload ? 'identical request' : 'a variant of this request'}</Badge>{' '}
                <span className="font-mono text-xs text-slate-500">{p.request_id}</span>
              </li>
            ))}
          </ul>
        </Card>
      )}

      <Card title="Approvals and rejections">
        {approvals.length === 0 ? (
          <p className="text-sm text-slate-500">No decisions recorded yet.</p>
        ) : (
          <ul className="flex flex-col gap-1 text-sm" aria-label="Approvals">
            {approvals.map((a) => (
              <li key={a.id}>
                <Badge tone={a.decision === 'approve' ? 'success' : 'danger'}>{a.decision === 'approve' ? 'Approved' : 'Rejected'}</Badge>{' '}
                by {actorLabel(a.decided_by, 'platform')} on {fmt(a.decided_at)} ({reasonLabel(a.reason_code)})
              </li>
            ))}
          </ul>
        )}
      </Card>

      {isPending && readiness && (
        <Card title="Current readiness">
          <ReadinessChecklist readiness={readiness} mode={r.action === 'close' ? 'close' : 'other'} />
        </Card>
      )}

      {isPending && (
        <div className="flex flex-wrap items-center gap-2">
          {controls.approve && <Button onClick={() => setDialog('approve')}>Approve</Button>}
          {controls.reject && (
            <Button variant="danger" onClick={() => setDialog('reject')}>
              Reject
            </Button>
          )}
          {controls.note && <p className="text-sm text-slate-600">{controls.note}</p>}
          {perms.launchRequest && isRequester && (
            <Button variant="secondary" onClick={() => setDialog('cancel')}>
              Cancel request
            </Button>
          )}
        </div>
      )}

      {dialog === 'approve' && (
        <ApproveDialog
          tenantId={tenantId}
          request={r}
          readiness={readiness}
          onClose={() => setDialog(null)}
          onStale={refresh}
          onDone={(res) => {
            setDialog(null)
            setNotice(
              res.executed
                ? `Approval recorded and the request executed: the status is now ${subjectStatusLabel(res.request.to_status)}.`
                : `Approval recorded (${(res.request.approvals ?? []).filter((a) => a.decision === 'approve').length} of ${res.request.required_approvals}). Further approvers are still needed.`,
            )
            refresh()
          }}
        />
      )}
      {dialog === 'reject' && (
        <RejectDialog
          tenantId={tenantId}
          request={r}
          onClose={() => setDialog(null)}
          onDone={() => {
            setDialog(null)
            setNotice('Rejection recorded. The request has ended.')
            refresh()
          }}
        />
      )}
      {dialog === 'cancel' && (
        <CancelDialog
          tenantId={tenantId}
          request={r}
          onClose={() => setDialog(null)}
          onDone={() => {
            setDialog(null)
            setNotice('Request cancelled.')
            refresh()
          }}
        />
      )}
    </div>
  )
}
