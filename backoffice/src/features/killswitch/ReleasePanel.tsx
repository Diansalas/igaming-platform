import { useMutation, useQuery } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { KILL_SWITCH_REASON_MAX, approveRelease, cancelRelease, getReleaseRequest, requestRelease, type KillSwitch } from '../../api/killSwitch'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { ActorText, EnumText, KillSwitchErrorAlert } from './KillSwitchBits'
import { CONSEQUENCE_RELEASE_REQUEST, REASON_HELP, describeRequestStatus, requestStatusTone } from './labels'

/**
 * Four-eyes release for one engaged switch. Whether the viewer is allowed is decided by the server only:
 * self-approval, a tenant caller on a platform-engaged switch, a stale version and an expired request all come
 * back as refusals that are shown through the closed error map. The hints here never disable the server path.
 */
export function ReleasePanel({
  tenantId,
  ks,
  viewerIsPlatform,
  viewerId,
  onChanged,
  onRefused,
  onNotice,
}: {
  tenantId: string
  ks: KillSwitch
  viewerIsPlatform: boolean
  viewerId: string | undefined
  onChanged: () => void
  onRefused: () => void
  onNotice: (msg: string) => void
}) {
  const [dialog, setDialog] = useState<'request' | 'approve' | 'cancel' | null>(null)
  const reqId = ks.release_request_id ?? null
  const rq = useQuery({
    queryKey: ['killswitch', 'release-request', tenantId, reqId],
    queryFn: () => getReleaseRequest(tenantId, reqId as string),
    enabled: reqId !== null && ks.engaged,
    retry: false,
  })
  const req = rq.data

  if (!ks.engaged) {
    return <p className="text-sm text-slate-600">This switch is released. Dispatch to the provider in scope is no longer blocked by it.</p>
  }
  const platformLockHint = !viewerIsPlatform && ks.engaged_by_scope === 'platform'

  return (
    <div className="flex flex-col gap-3">
      <h3 className="text-sm font-semibold text-slate-900">Release (four-eyes)</h3>
      {platformLockHint && (
        <p className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
          This switch was engaged by platform staff. The server lets only platform staff release it, and will refuse a release request from tenant staff.
        </p>
      )}
      {reqId === null && (
        <>
          <p className="text-sm text-slate-600">No release request is open. {CONSEQUENCE_RELEASE_REQUEST}</p>
          <div>
            <Button variant="secondary" onClick={() => { onNotice(''); setDialog('request') }}>
              Request release
            </Button>
          </div>
        </>
      )}
      {reqId !== null && rq.isLoading && <LoadingSpinner label="Loading release request..." />}
      {reqId !== null && rq.error && <KillSwitchErrorAlert error={rq.error} op="get_request" />}
      {req && (
        <section aria-label="Release request" className="rounded-md border border-border bg-surface-alt p-3">
          <p className="mb-2 text-xs text-slate-500">This is the request exactly as it was filed. Approval acts on this frozen content only.</p>
          <dl className="grid grid-cols-[11rem_1fr] gap-y-1 text-sm">
            <dt className="text-slate-500">Status</dt>
            <dd>
              <Badge tone={requestStatusTone(req.status)}>
                <EnumText view={describeRequestStatus(req.status)} />
              </Badge>
            </dd>
            <dt className="text-slate-500">Switch version it applies to</dt>
            <dd>{req.expected_version}</dd>
            <dt className="text-slate-500">Reason code</dt>
            <dd className="font-mono text-xs">{req.reason_code}</dd>
            <dt className="text-slate-500">Requested by</dt>
            <dd><ActorText id={req.requested_by} scope={req.requested_by_scope} /></dd>
            <dt className="text-slate-500">Filed</dt>
            <dd>{new Date(req.created_at).toLocaleString()}</dd>
            <dt className="text-slate-500">Expires</dt>
            <dd>{new Date(req.expires_at).toLocaleString()}</dd>
            {req.approved_by_scope != null || req.approved_by != null ? (
              <>
                <dt className="text-slate-500">Approved by</dt>
                <dd><ActorText id={req.approved_by} scope={req.approved_by_scope} /></dd>
              </>
            ) : (
              <>
                <dt className="text-slate-500">Approved by</dt>
                <dd>Not approved yet</dd>
              </>
            )}
          </dl>
          {req.status === 'open' && req.expected_version !== ks.version && (
            <p className="mt-2 text-sm text-amber-900">The switch is now at version {ks.version}, not {req.expected_version}. The server is expected to refuse approval; cancel this request and file a new one.</p>
          )}
          {req.status === 'open' && viewerId && req.requested_by === viewerId && (
            <p className="mt-2 text-sm text-amber-900">You filed this request. A different person must approve it; the server refuses self-approval.</p>
          )}
          {req.status === 'open' && (
            <div className="mt-3 flex gap-2">
              <Button onClick={() => { onNotice(''); setDialog('approve') }}>Approve and release</Button>
              <Button variant="secondary" onClick={() => { onNotice(''); setDialog('cancel') }}>Cancel request</Button>
            </div>
          )}
          {req.status !== 'open' && <p className="mt-2 text-sm text-slate-600">This request is no longer open, so it cannot be approved or cancelled.</p>}
        </section>
      )}
      {dialog === 'request' && (
        <RequestDialog
          tenantId={tenantId}
          ks={ks}
          onClose={() => setDialog(null)}
          onRefused={onRefused}
          onDone={() => { setDialog(null); onNotice('Release requested. The switch is still engaged until a different person approves.'); onChanged() }}
        />
      )}
      {(dialog === 'approve' || dialog === 'cancel') && req && (
        <DecideDialog
          op={dialog}
          tenantId={tenantId}
          requestId={req.id}
          expectedVersion={req.expected_version}
          onClose={() => setDialog(null)}
          onRefused={onRefused}
          onDone={() => {
            const was = dialog
            setDialog(null)
            onNotice(was === 'approve' ? 'Approved. The server released the switch. No funds were moved and no withdrawal was changed.' : 'Release request cancelled. The switch stays engaged.')
            onChanged()
          }}
        />
      )}
    </div>
  )
}

function RequestDialog({ tenantId, ks, onClose, onDone, onRefused }: { tenantId: string; ks: KillSwitch; onClose: () => void; onDone: () => void; onRefused: () => void }) {
  const [reason, setReason] = useState('')
  const busy = useRef(false)
  const m = useMutation({
    mutationFn: () => requestRelease(tenantId, ks.id, reason),
    onSuccess: onDone,
    onError: () => onRefused(),
    onSettled: () => {
      busy.current = false
    },
  })
  const valid = reason.trim() !== '' && reason.length <= KILL_SWITCH_REASON_MAX
  return (
    <Modal title="Request kill switch release" onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-slate-600">{CONSEQUENCE_RELEASE_REQUEST}</p>
        <p className="text-sm text-slate-600">It will apply to switch version {ks.version}. If the switch changes before approval, the server refuses the approval.</p>
        <TextInput label="Reason code (required)" value={reason} onChange={(e) => setReason(e.target.value)} help={REASON_HELP} maxLength={KILL_SWITCH_REASON_MAX} autoComplete="off" />
        <KillSwitchErrorAlert error={m.error} op="request" />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button disabled={!valid} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>
            File release request
          </Button>
        </div>
      </div>
    </Modal>
  )
}

function DecideDialog({
  op,
  tenantId,
  requestId,
  expectedVersion,
  onClose,
  onDone,
  onRefused,
}: {
  op: 'approve' | 'cancel'
  tenantId: string
  requestId: string
  expectedVersion: number
  onClose: () => void
  onDone: () => void
  onRefused: () => void
}) {
  const busy = useRef(false)
  const m = useMutation({
    mutationFn: async (): Promise<void> => {
      if (op === 'approve') await approveRelease(tenantId, requestId)
      else await cancelRelease(tenantId, requestId)
    },
    onSuccess: onDone,
    onError: () => onRefused(),
    onSettled: () => {
      busy.current = false
    },
  })
  return (
    <Modal title={op === 'approve' ? 'Approve and release' : 'Cancel release request'} onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-slate-600">
          {op === 'approve'
            ? `Approving releases the switch, in the same step, if it is still at version ${expectedVersion}. Releasing lets dispatch to the provider in scope resume. It does not move funds and does not change any withdrawal. You cannot approve a request you filed yourself.`
            : 'Cancelling leaves the switch engaged and closes this request. A new request can be filed afterwards.'}
        </p>
        <KillSwitchErrorAlert error={m.error} op={op} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Back</Button>
          <Button variant={op === 'approve' ? 'danger' : 'primary'} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>
            {op === 'approve' ? 'Approve and release' : 'Cancel request'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
