import { useMutation } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import {
  approveLaunchRequest,
  cancelLaunchRequest,
  rejectLaunchRequest,
  suspendSubject,
  type LaunchDecisionResult,
  type LaunchReadiness,
  type LaunchRequest,
  type LaunchRequestEnvelope,
  type LaunchResult,
} from '../../api/launch'
import { Button } from '../../components/Button'
import { SelectField } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { useQuery } from '@tanstack/react-query'
import { getLaunchReadiness } from '../../api/launch'
import { FundsView, LaunchDisclaimer, LaunchErrorAlert, ReadinessChecklist, WarningBox } from './LaunchBits'
import {
  APPROVE_REASON_CODES,
  CANCEL_REASON_CODES,
  CONDITIONS_NOTE_WARNING,
  FUNDS_INACCESSIBLE_NOTICE,
  REJECT_REASON_CODES,
  REQUEST_REASON_CODES,
  actionLabel,
} from './labels'

/** Guards a double click: a second submit while the first is in flight is dropped. */
function useOnceSubmit() {
  const busy = useRef(false)
  return {
    run(fn: () => void) {
      if (busy.current) return
      busy.current = true
      fn()
    },
    release() {
      busy.current = false
    },
  }
}

export interface ApproveDialogProps {
  tenantId: string
  request: LaunchRequest
  /** The CURRENT readiness from the pending GET: its hash is the seen_readiness_hash. */
  readiness?: LaunchReadiness
  onClose: () => void
  onDone: (r: LaunchDecisionResult) => void
  /** Called after a stale-readiness refusal so the caller can re-read the pending GET. */
  onStale: () => void
}

export function ApproveDialog({ tenantId, request, readiness: liveReadiness, onClose, onDone, onStale }: ApproveDialogProps) {
  // Frozen at open: a background refetch must never swap the checks/hash the approver is looking at.
  const [readiness] = useState(() => liveReadiness)
  const [reason, setReason] = useState('')
  const once = useOnceSubmit()
  const m = useMutation({
    mutationFn: () => approveLaunchRequest(tenantId, request.id, { reason_code: reason, seen_readiness_hash: readiness?.readiness_hash ?? '' }),
    onSuccess: onDone,
    onError: (e) => {
      if ((e as { code?: string }).code === 'LAUNCH_STALE_READINESS') onStale()
    },
    onSettled: once.release,
  })
  const blocked = !readiness || !reason
  return (
    <Modal title="Approve launch request" onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <LaunchDisclaimer />
        <p className="text-sm text-slate-600">
          You are approving: <strong>{actionLabel(request.action)}</strong> ({request.from_status} to {request.to_status}). Your approval is
          recorded against the readiness shown below. If it changes before you submit, nothing is recorded and you must review again.
        </p>
        {readiness ? (
          <ReadinessChecklist readiness={readiness} mode={request.action === 'close' ? 'close' : 'other'} />
        ) : (
          <p className="text-sm text-red-700">The current readiness is not available, so this request cannot be approved from here. Refresh and try again.</p>
        )}
        <SelectField label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} options={[{ value: '', label: 'Select a reason' }, ...APPROVE_REASON_CODES]} />
        <LaunchErrorAlert error={m.error} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>
            Cancel
          </Button>
          <Button onClick={() => once.run(() => m.mutate())} disabled={blocked} isLoading={m.isPending}>
            Approve request
          </Button>
        </div>
      </div>
    </Modal>
  )
}

export function RejectDialog({
  tenantId,
  request,
  onClose,
  onDone,
}: {
  tenantId: string
  request: LaunchRequest
  onClose: () => void
  onDone: (r: LaunchDecisionResult) => void
}) {
  const [reason, setReason] = useState('')
  const once = useOnceSubmit()
  const m = useMutation({
    mutationFn: () => rejectLaunchRequest(tenantId, request.id, { reason_code: reason }),
    onSuccess: onDone,
    onSettled: once.release,
  })
  return (
    <Modal title="Reject launch request" onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-slate-600">
          A recorded rejection ends this request. An identical new request is refused for the request window (up to 72 hours).
        </p>
        <SelectField label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} options={[{ value: '', label: 'Select a reason' }, ...REJECT_REASON_CODES]} />
        <LaunchErrorAlert error={m.error} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>
            Cancel
          </Button>
          <Button variant="danger" onClick={() => once.run(() => m.mutate())} disabled={!reason} isLoading={m.isPending}>
            Reject request
          </Button>
        </div>
      </div>
    </Modal>
  )
}

export function CancelDialog({
  tenantId,
  request,
  onClose,
  onDone,
}: {
  tenantId: string
  request: LaunchRequest
  onClose: () => void
  onDone: (r: LaunchRequestEnvelope) => void
}) {
  const [reason, setReason] = useState('')
  const once = useOnceSubmit()
  const m = useMutation({
    mutationFn: () => cancelLaunchRequest(tenantId, request.id, { reason_code: reason }),
    onSuccess: onDone,
    onSettled: once.release,
  })
  return (
    <Modal title="Cancel launch request" onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-slate-600">Only the requester can cancel, and only while the request is pending.</p>
        <SelectField label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} options={[{ value: '', label: 'Select a reason' }, ...CANCEL_REASON_CODES]} />
        <LaunchErrorAlert error={m.error} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>
            Keep request
          </Button>
          <Button variant="danger" onClick={() => once.run(() => m.mutate())} disabled={!reason} isLoading={m.isPending}>
            Cancel request
          </Button>
        </div>
      </div>
    </Modal>
  )
}

export interface SuspendDialogProps {
  tenantId: string
  brandId?: string
  subjectLabel: string
  onClose: () => void
  onDone: (r: LaunchResult) => void
}

/**
 * Single-actor emergency suspension (fail-closed direction, ADR 0112 6.3): executes at once with a
 * mandatory reason code. Reactivation is always four-eyes. Suspension-specific funds notice:
 * funds inaccessible, not stranded.
 */
export function SuspendDialog({ tenantId, brandId, subjectLabel, onClose, onDone }: SuspendDialogProps) {
  const [reason, setReason] = useState('')
  const [note, setNote] = useState('')
  const once = useOnceSubmit()
  const readinessQuery = useQuery({
    queryKey: ['launch-readiness', tenantId, 'suspend', brandId ?? null],
    queryFn: () => getLaunchReadiness(tenantId, { action: 'suspend', brand_id: brandId }),
    retry: false,
  })
  const m = useMutation({
    mutationFn: () => suspendSubject(tenantId, brandId, { reason_code: reason, ...(note.trim() ? { conditions_note: note.trim() } : {}) }),
    onSuccess: onDone,
    onSettled: once.release,
  })
  const noteMissing = reason === 'other_documented' && note.trim() === ''
  const readiness = readinessQuery.data?.readiness
  return (
    <Modal title={`Suspend ${subjectLabel}`} onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <LaunchDisclaimer />
        <p className="text-sm font-medium text-amber-800">
          Emergency action: one authorised person suspends immediately, with a mandatory reason. It supersedes any pending request for this subject.
          Reactivation always needs four-eyes approval.
        </p>
        <p className="text-sm font-medium text-amber-800">{FUNDS_INACCESSIBLE_NOTICE}</p>
        {readinessQuery.isLoading && <LoadingSpinner label="Loading player funds..." />}
        {readinessQuery.error && <LaunchErrorAlert error={readinessQuery.error} />}
        {readiness && <FundsView readiness={readiness} mode="suspend" hideModeNotice />}
        <SelectField label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} options={[{ value: '', label: 'Select a reason' }, ...REQUEST_REASON_CODES.suspend]} />
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium text-slate-700">Conditions / notes{reason === 'other_documented' ? ' (required)' : ' (optional)'} - record only, not enforced - record only, not enforced</span>
          <WarningBox>{CONDITIONS_NOTE_WARNING}</WarningBox>
          <textarea
            aria-label="Conditions note"
            className="rounded-md border border-border px-3 py-2 text-sm shadow-sm"
            rows={3}
            maxLength={2000}
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
        </label>
        <LaunchErrorAlert error={m.error} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>
            Cancel
          </Button>
          <Button variant="danger" onClick={() => once.run(() => m.mutate())} disabled={!reason || noteMissing} isLoading={m.isPending}>
            Suspend now
          </Button>
        </div>
      </div>
    </Modal>
  )
}
