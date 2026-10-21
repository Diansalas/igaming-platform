import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  approveLicensingChange,
  cancelLicensingChange,
  getLicensingChange,
  isReasonCode,
  rejectLicensingChange,
  type LicensingChange,
  type LicensingChangeResponse,
} from '../../api/licensing'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { BasisPayloadSummary } from './BasisBits'
import { MARKET_CHANGE_KINDS, MarketPayloadSummary } from './MarketBits'
import { ApprovalRules, ChangeStatusBadge, Disclaimer, LicensingErrorAlert, Pending, Success, short } from './LicensingBits'
import { REASON_CODE_HELP, kindLabel, safeToken, when } from './labels'

const DAY_MS = 24 * 60 * 60 * 1000

type Op = 'approve' | 'reject' | 'cancel'

/** Display-only classification against the local clock; the database decides whether an approval counts. */
export function approvalIsOld(decidedAt: string, now: number = Date.now()): boolean {
  const t = Date.parse(decidedAt)
  return !Number.isNaN(t) && now - t > DAY_MS
}

function DecisionDialog({ op, change, onClose, onDone }: { op: Op; change: LicensingChange; onClose: () => void; onDone: (r: LicensingChangeResponse) => void }) {
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const needsReason = op !== 'cancel'
  const valid = !needsReason || isReasonCode(reason)
  const m = useMutation({
    mutationFn: () => {
      if (op === 'approve') return approveLicensingChange(change.id, reason)
      if (op === 'reject') return rejectLicensingChange(change.id, reason)
      return cancelLicensingChange(change.id)
    },
    onSuccess: onDone,
    onSettled: () => {
      sending.current = false
    },
  })
  function submit() {
    if (!valid || sending.current) return
    sending.current = true
    m.mutate()
  }
  const title = op === 'approve' ? 'Approve licensing change' : op === 'reject' ? 'Reject licensing change' : 'Cancel licensing change'
  return (
    <Modal title={title} onClose={onClose}>
      <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); submit() }}>
        <p className="text-sm">
          {kindLabel(change.kind)} - request <span className="font-mono text-xs">{short(change.id)}</span>
        </p>
        <Disclaimer />
        {op === 'approve' && (
          <p className="text-sm text-slate-700">
            Check the content of the request below against the change ticket, not against memory. {change.required_approvals > 1 ? `This change needs ${change.required_approvals} approvals from different people; the final approval executes it.` : 'The server will execute the change once enough approvals exist.'} Approving needs a fresh second-factor check.
          </p>
        )}
        {op === 'reject' && <p className="text-sm text-slate-700">Rejecting is final for this request. No second-factor check is needed.</p>}
        {op === 'cancel' && <p className="text-sm text-slate-700">Only the person who filed the request can cancel it. Cancelling is final. No reason code is taken and no second-factor check is needed.</p>}
        {needsReason && (
          <>
            <TextInput label="Reason code" value={reason} onChange={(e) => setReason(e.target.value.trim())} autoComplete="off" maxLength={64} help={REASON_CODE_HELP} />
            {reason !== '' && !isReasonCode(reason) && <p role="alert" className="text-sm text-red-700">That is not a valid reason code.</p>}
          </>
        )}
        {m.error && <LicensingErrorAlert error={m.error} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" variant={op === 'approve' ? 'primary' : 'danger'} disabled={!valid} isLoading={m.isPending}>
            {op === 'approve' ? 'Approve' : op === 'reject' ? 'Reject' : 'Cancel request'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

export function ChangeDetailPage() {
  const { changeId = '' } = useParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<Op | null>(null)
  const [outcome, setOutcome] = useState<{ op: Op; r: LicensingChangeResponse } | null>(null)
  const q = useQuery({ queryKey: ['licensing', 'change', changeId], queryFn: () => getLicensingChange(changeId), enabled: !!changeId, retry: false })

  if (q.isLoading) return <LoadingSpinner label="Loading request..." />
  if (q.error) return <LicensingErrorAlert error={q.error} />
  const c = q.data?.change_request
  if (!c) return null

  const sub = claims?.sub
  const own = !!sub && c.requested_by === sub
  const mine = !!sub && c.approvals.some((a) => a.approver_id === sub)
  const open = c.status === 'requested'
  const expired = Date.parse(c.expires_at) <= Date.now()
  const stale = c.approvals.filter((a) => approvalIsOld(a.decided_at)).length

  function done(op: Op, r: LicensingChangeResponse) {
    setDialog(null)
    setOutcome({ op, r })
    void qc.invalidateQueries({ queryKey: ['licensing'] })
  }

  return (
    <div className="flex flex-col gap-4">
      <Disclaimer />
      <Card title={`${kindLabel(c.kind)} - request ${short(c.id)}`}>
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Status</dt>
            <dd className="mt-1"><ChangeStatusBadge status={c.status} /></dd>
          </div>
          <div>
            <dt className="text-slate-500">Class (computed by the database)</dt>
            <dd className="mt-1">{c.widening ? 'Widening' : 'Narrowing or neutral'}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Approvals</dt>
            <dd className="mt-1">{c.approvals.length} of {c.required_approvals} required</dd>
          </div>
          <div>
            <dt className="text-slate-500">Request expires</dt>
            <dd className="mt-1">{when(c.expires_at)}{open && expired ? ' (expired)' : ''}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Filed</dt>
            <dd className="mt-1">{when(c.created_at)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Requested by</dt>
            <dd className="mt-1">{c.requested_by ? `Platform staff (${short(c.requested_by)})${own ? ' - you' : ''}` : 'Not shown'}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Reason code</dt>
            <dd className="mt-1 font-mono text-xs">{safeToken(c.reason_code)}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Payload hash</dt>
            <dd className="mt-1 break-all font-mono text-xs">{safeToken(c.payload_hash)}</dd>
          </div>
        </dl>
        {c.note ? <p className="mt-3 whitespace-pre-wrap text-sm text-slate-700">Note: {c.note}</p> : null}
        {c.status === 'executed' && <p className="mt-3 text-sm">Executed {when(c.executed_at)}.</p>}
        {c.decision_reason_code && <p className="mt-3 text-sm">Decision reason code: <span className="font-mono text-xs">{safeToken(c.decision_reason_code)}</span></p>}
      </Card>

      {c.kind.startsWith('operating_basis_') && (
        <Card title="Operating basis in words">
          <BasisPayloadSummary kind={c.kind} payload={c.payload} />
          {q.data?.outcome && typeof q.data.outcome.operating_basis_id === 'string' && /^[0-9a-fA-F-]{36}$/.test(q.data.outcome.operating_basis_id) && (
            <p className="mt-2 text-sm">
              <Link className="text-brand-700 hover:underline" to={`/licensing/operating-bases/${encodeURIComponent(q.data.outcome.operating_basis_id)}`}>Open the resulting basis version</Link>
            </p>
          )}
        </Card>
      )}

      {MARKET_CHANGE_KINDS.includes(c.kind) && (
        <Card title="Market configuration in words">
          <MarketPayloadSummary kind={c.kind} payload={c.payload} widening={c.widening} />
        </Card>
      )}

      <Card title="Content of the request (frozen at filing)">
        <pre aria-label="Request payload" className="overflow-x-auto rounded-md bg-surface-alt p-3 text-xs">{JSON.stringify(c.payload, null, 2)}</pre>
        <p className="mt-2 text-xs text-slate-600">If this names a licence evidence reference, it is a reference supplied by the operator and is not verified by the platform.</p>
      </Card>

      <Card title="Approvals">
        {c.approvals.length === 0 ? (
          <p className="text-sm text-slate-600">No approvals yet.</p>
        ) : (
          <ul className="flex flex-col gap-1 text-sm">
            {c.approvals.map((a, i) => (
              <li key={`${a.decided_at}-${i}`}>
                {a.approver_id ? `Platform staff (${short(a.approver_id)})${sub && a.approver_id === sub ? ' - you' : ''}` : 'Platform staff'} - {when(a.decided_at)} - reason <span className="font-mono text-xs">{safeToken(a.reason_code)}</span>
                {approvalIsOld(a.decided_at) ? <span className="ml-2 text-amber-800">older than 24 hours</span> : null}
              </li>
            ))}
          </ul>
        )}
        {open && stale > 0 && <p className="mt-2 text-sm text-amber-900">An approval older than 24 hours does not count at execution. If this request cannot be executed, cancel it and file a new one.</p>}
      </Card>

      {c.widening && <ApprovalRules />}

      {outcome && <OutcomeNotice op={outcome.op} r={outcome.r} />}

      {open && (
        <div className="flex flex-wrap gap-2" aria-label="Decisions">
          {perms.licensingApprove && (
            <>
              <Button disabled={own || mine || expired} title={own ? 'You filed this request; a different person must approve it.' : mine ? 'You already approved this request.' : undefined} onClick={() => { setOutcome(null); setDialog('approve') }}>Approve</Button>
              <Button variant="secondary" disabled={own || expired} title={own ? 'You filed this request; a different person must decide it.' : undefined} onClick={() => { setOutcome(null); setDialog('reject') }}>Reject</Button>
            </>
          )}
          {perms.licensingRequest && own && <Button variant="secondary" onClick={() => { setOutcome(null); setDialog('cancel') }}>Cancel request</Button>}
        </div>
      )}
      {open && own && <p className="text-sm text-slate-700">You filed this request. A different person must approve it; you can cancel it.</p>}
      {open && mine && <p className="text-sm text-slate-700">You already approved this request. A second approval must come from a different person.</p>}
      <p className="text-xs text-slate-500">These buttons are hints. The server decides who may approve, and every refusal is shown.</p>

      <Link to="/licensing/changes" className="text-sm text-brand-700 hover:underline">&larr; Back to licensing changes</Link>
      {dialog && <DecisionDialog op={dialog} change={c} onClose={() => setDialog(null)} onDone={(r) => done(dialog, r)} />}
    </div>
  )
}

function OutcomeNotice({ op, r }: { op: Op; r: LicensingChangeResponse }) {
  const c = r.change_request
  if (op === 'reject') return <Success>Rejected. The request is closed and nothing was changed.</Success>
  if (op === 'cancel') return <Success>Cancelled. The request is closed and nothing was changed.</Success>
  if (r.executed && c.status === 'executed') return <Success>Approval recorded and the change was executed.</Success>
  if (c.status === 'requested' || c.status === 'approved') {
    return <Pending>Approval recorded: {c.approvals.length} of {c.required_approvals}. Still pending approval - the change has not taken effect.</Pending>
  }
  return <Pending>The server answered with status {safeToken(c.status)}. Check the request below before relying on it.</Pending>
}
