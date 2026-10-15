import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { approveForceResolution, cancelForceResolution, listForceResolutions, rejectForceResolution, type PaymentForceResolution } from '../../api/forceResolutions'
import { getNavPermissions } from '../../auth/permissions'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { formatAmountBoth } from '../../lib/minorUnits'
import { reasonByteLength } from '../../api/killSwitch'
import { ConsoleErrorAlert, Hint, MockPaymentsBanner, Notice, actorText, isUuid, shortId, useConsoleTenant } from '../consoles/ConsoleBits'
import { describeRefusalCode } from '../consoles/errors'
import { EnumText } from '../killswitch/KillSwitchBits'
import { safeCode, type EnumView } from '../killswitch/labels'
import { describeKind } from '../payments/labels'
import { CancelDialog, DecisionDialog } from './DecisionDialogs'
import { ForceRequestDialog } from './ForceRequestDialog'

const STATES: Record<string, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Pending approval', tone: 'warning' },
  executing: { label: 'Executing', tone: 'info' },
  executed: { label: 'Executed', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'neutral' },
  cancelled: { label: 'Cancelled', tone: 'neutral' },
  expired: { label: 'Expired', tone: 'neutral' },
  refused_at_execution: { label: 'Refused at execution', tone: 'danger' },
}
export function describeResolutionState(v: unknown): EnumView & { tone: BadgeTone } {
  if (typeof v === 'string' && Object.prototype.hasOwnProperty.call(STATES, v)) return { label: STATES[v].label, known: true, tone: STATES[v].tone }
  return { label: 'Unrecognised', code: safeCode(v), known: false, tone: 'danger' }
}

type Dialog = { op: 'request' } | { op: 'approve' | 'reject' | 'cancel'; snap: PaymentForceResolution }

/**
 * K3 force-resolution console (ADR 0114 S8, M1/M2 only; M4 is a separate MOCK-only slice). Lists the tenant's
 * resolutions (the API returns the 100 most recent, no cursor), shows the frozen content an approver decides on,
 * and fronts propose / approve / reject / cancel on the existing routes. Authorization is the server's: the
 * buttons are hidden for read-only roles and disabled for the caller's own request as hints only, and every
 * refusal is rendered through the closed code map.
 */
export function ForceResolutionsPage() {
  const { claims, platform, tenantId, tenantLabel, picker, searchParams, setSearchParams } = useConsoleTenant()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [notice, setNotice] = useState('')
  const selectedId = searchParams.get('resolution') ?? ''
  const prefill = searchParams.get('attempt') ?? ''

  const list = useQuery({ queryKey: ['force', 'list', tenantId], queryFn: () => listForceResolutions(tenantId), enabled: tenantId !== '', retry: false })
  const rows = list.data?.items ?? []
  const selected = rows.find((r) => r.id === selectedId) ?? null
  const refresh = () => void qc.invalidateQueries({ queryKey: ['force'] })
  function select(id: string) {
    const next = new URLSearchParams(searchParams)
    if (id) next.set('resolution', id)
    else next.delete('resolution')
    setSearchParams(next, { replace: true })
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Force resolutions" description="Four-eyes resolution of parked payments (M1 deposit evidence, M2 payout paid or not paid). Nothing runs until the last required approval is recorded." />
      <MockPaymentsBanner />
      {picker}
      {tenantId === '' && platform && <p className="text-sm text-slate-500">Choose a tenant to see its force-resolutions.</p>}
      {tenantId !== '' && (
        <>
          <div className="flex gap-2">
            {perms.paymentForceResolutionAct && <Button onClick={() => { setNotice(''); setDialog({ op: 'request' }) }}>Propose a force-resolution</Button>}
            <Button variant="secondary" onClick={refresh} isLoading={list.isFetching}>Refresh</Button>
          </div>
          {!perms.paymentForceResolutionAct && <p className="text-sm text-slate-600">Your role can read force-resolutions but cannot propose or decide them.</p>}
          {notice && <Notice>{notice}</Notice>}
          {list.isLoading && <LoadingSpinner label="Loading force-resolutions..." />}
          {list.error && <ConsoleErrorAlert error={list.error} family="k3" op="list" />}
          {list.data && rows.length === 0 && <p className="text-sm text-slate-600">No force-resolutions for this tenant.</p>}
          {rows.length > 0 && (
            <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
              <table className="w-full text-left text-sm">
                <caption className="sr-only">Force-resolutions</caption>
                <thead className="bg-surface-alt text-xs uppercase tracking-wide text-slate-500">
                  <tr>{['Filed', 'Kind', 'State', 'Amount', 'Requested by', 'Approvals needed', 'Expires', ''].map((h) => <th key={h || 'a'} scope="col" className="px-3 py-2 font-medium">{h}</th>)}</tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {rows.map((r) => {
                    const s = describeResolutionState(r.state)
                    return (
                      <tr key={r.id} className={r.id === selectedId ? 'bg-brand-50' : undefined}>
                        <td className="px-3 py-2">{new Date(r.created_at).toLocaleString()}</td>
                        <td className="px-3 py-2"><EnumText view={describeKind(r.kind)} /></td>
                        <td className="px-3 py-2"><Badge tone={s.tone}><EnumText view={s} /></Badge></td>
                        <td className="px-3 py-2 whitespace-nowrap">{formatAmountBoth(r.amount_minor_units, r.exponent, r.asset_code)}</td>
                        <td className="px-3 py-2">{actorText(r.requested_by, r.requested_by_scope)}</td>
                        <td className="px-3 py-2">{r.required_at_submission}</td>
                        <td className="px-3 py-2">{new Date(r.expires_at).toLocaleString()}</td>
                        <td className="px-3 py-2"><Button variant="secondary" onClick={() => select(r.id)} aria-label={`Open resolution ${shortId(r.id)}`}>Details</Button></td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
          {list.data && <p className="text-xs text-slate-500">The API returns the 100 most recent resolutions for the tenant and has no cursor or filter, so older ones are not reachable here.</p>}
          {isUuid(selectedId) && !selected && list.data && <Hint>The selected resolution is not among the 100 most recent.</Hint>}
          {selected && (
            <Detail
              r={selected}
              viewerId={claims?.sub}
              canAct={perms.paymentForceResolutionAct}
              onClose={() => select('')}
              onOpen={(op) => { setNotice(''); setDialog({ op, snap: selected }) }}
            />
          )}
        </>
      )}
      {dialog?.op === 'request' && (
        <ForceRequestDialog
          tenantId={tenantId}
          tenantLabel={tenantLabel}
          prefillAttempt={prefill}
          onClose={() => setDialog(null)}
          onRefresh={refresh}
          onDone={(r) => { setDialog(null); setNotice('Force-resolution proposed. It is pending until the required approvals are recorded.'); select(r.id) }}
        />
      )}
      {dialog && dialog.op !== 'request' && (dialog.op === 'approve' || dialog.op === 'reject') && (
        <DecisionDialog
          op={dialog.op}
          title={dialog.op === 'approve' ? 'Approve force-resolution' : 'Reject force-resolution'}
          family="k3"
          tenantId={tenantId}
          tenantLabel={tenantLabel}
          frozenHash={dialog.snap.payload_hash}
          liveHash={rows.find((x) => x.id === dialog.snap.id)?.payload_hash}
          reasonOk={(s) => s.trim() !== '' && reasonByteLength(s) <= 64}
          reasonHelp="1 to 64 BYTES (the server counts bytes; non-ASCII characters take more than one), recorded in the audit log."
          summary={[
            ['Attempt id', <span key="a" className="font-mono text-xs">{dialog.snap.attempt_id}</span>],
            ['Operation', safeCode(dialog.snap.operation)],
            ['Kind', <EnumText key="k" view={describeKind(dialog.snap.kind)} />],
            ['Finding', dialog.snap.finding_code ? safeCode(dialog.snap.finding_code) : 'Not applicable'],
            ['Basis', dialog.snap.basis_code ? safeCode(dialog.snap.basis_code) : 'Not applicable'],
            ['Context', dialog.snap.context_code ? safeCode(dialog.snap.context_code) : 'Not applicable'],
            ['Evidence reference hash', <span key="e" className="break-all font-mono text-xs">{dialog.snap.evidence_ref_hash ?? 'None'}</span>],
            ['Reason code on the request', <span key="r" className="font-mono text-xs">{dialog.snap.reason_code}</span>],
            ['Amount', formatAmountBoth(dialog.snap.amount_minor_units, dialog.snap.exponent, dialog.snap.asset_code)],
            ['Target state', dialog.snap.target_state ? safeCode(dialog.snap.target_state) : 'Not applicable'],
            ['Requested by', actorText(dialog.snap.requested_by, dialog.snap.requested_by_scope)],
            ['Approvals needed', String(dialog.snap.required_at_submission)],
            ['Expires', new Date(dialog.snap.expires_at).toLocaleString()],
          ]}
          consequence={
            dialog.op === 'approve'
              ? 'Approving records your approval against this exact content. If it is the last approval required, the resolution executes in the same step and the parked payment is settled as described. A different person must give each approval; the server refuses your own.'
              : 'Rejecting records your rejection against this exact content and closes the resolution. Nothing is executed.'
          }
          send={(hash, reason, key) => (dialog.op === 'approve' ? approveForceResolution : rejectForceResolution)(tenantId, dialog.snap.id, hash, reason, key)}
          onClose={() => setDialog(null)}
          onDone={() => setDialog(null)}
          onRefused={refresh}
        />
      )}
      {dialog?.op === 'cancel' && (
        <CancelDialog
          title="Cancel force-resolution request"
          family="k3"
          tenantId={tenantId}
          tenantLabel={tenantLabel}
          detail="Cancelling closes this pending request. Nothing was executed. Only the person who filed it can cancel it."
          send={(key) => cancelForceResolution(tenantId, dialog.snap.id, key)}
          onClose={() => setDialog(null)}
          onDone={() => { setDialog(null); setNotice('Force-resolution request cancelled.') ; refresh() }}
          onRefused={refresh}
        />
      )}
    </div>
  )
}

function Detail({ r, viewerId, canAct, onClose, onOpen }: { r: PaymentForceResolution; viewerId: string | undefined; canAct: boolean; onClose: () => void; onOpen: (op: 'approve' | 'reject' | 'cancel') => void }) {
  const s = describeResolutionState(r.state)
  const pending = r.state === 'pending'
  const own = !!viewerId && r.requested_by === viewerId
  return (
    <section aria-label="Resolution detail" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
      <div className="flex items-start justify-between gap-2">
        <h2 className="text-sm font-semibold text-slate-900">Resolution detail</h2>
        <button type="button" className="text-xs text-slate-500 hover:underline" onClick={onClose}>Close detail</button>
      </div>
      <dl className="mt-3 grid grid-cols-[13rem_1fr] gap-y-1 text-sm">
        <dt className="text-slate-500">Resolution id</dt><dd className="font-mono text-xs">{r.id}</dd>
        <dt className="text-slate-500">Attempt</dt>
        <dd><Link className="font-mono text-xs text-brand-700 underline" to={`/parked-payments?tenant=${encodeURIComponent(r.tenant_id)}&attempt=${encodeURIComponent(r.attempt_id)}`}>{r.attempt_id}</Link></dd>
        <dt className="text-slate-500">Kind</dt><dd><EnumText view={describeKind(r.kind)} /></dd>
        <dt className="text-slate-500">State</dt><dd><Badge tone={s.tone}><EnumText view={s} /></Badge></dd>
        {r.refusal_code && (<><dt className="text-slate-500">Refusal</dt><dd>{describeRefusalCode(r.refusal_code)}</dd></>)}
        <dt className="text-slate-500">Amount</dt><dd>{formatAmountBoth(r.amount_minor_units, r.exponent, r.asset_code)}</dd>
        <dt className="text-slate-500">Target state</dt><dd>{r.target_state ? safeCode(r.target_state) : 'Not applicable'}</dd>
        {r.finding_code && (<><dt className="text-slate-500">Finding</dt><dd className="font-mono text-xs">{safeCode(r.finding_code)}</dd></>)}
        {r.basis_code && (<><dt className="text-slate-500">Basis</dt><dd className="font-mono text-xs">{safeCode(r.basis_code)}</dd></>)}
        {r.context_code && (<><dt className="text-slate-500">Context</dt><dd className="font-mono text-xs">{safeCode(r.context_code)}</dd></>)}
        <dt className="text-slate-500">Evidence reference hash</dt><dd className="break-all font-mono text-xs">{r.evidence_ref_hash ?? 'None'}</dd>
        <dt className="text-slate-500">Reason code</dt><dd className="font-mono text-xs">{r.reason_code}</dd>
        <dt className="text-slate-500">Requested by</dt><dd>{actorText(r.requested_by, r.requested_by_scope)}</dd>
        <dt className="text-slate-500">Approvals needed</dt><dd>{r.required_at_submission}</dd>
        <dt className="text-slate-500">Tenant status when filed</dt><dd className="font-mono text-xs">{safeCode(r.tenant_status_at_submission)}</dd>
        <dt className="text-slate-500">Expires</dt><dd>{new Date(r.expires_at).toLocaleString()}</dd>
        {r.ledger_transaction_id && (<><dt className="text-slate-500">Ledger transaction</dt><dd className="font-mono text-xs">{r.ledger_transaction_id}</dd></>)}
        <dt className="text-slate-500">Payload hash</dt><dd className="break-all font-mono text-xs">{r.payload_hash}</dd>
      </dl>
      {pending && canAct && (
        <div className="mt-4 flex flex-col gap-2">
          {own && <Hint>You filed this request. A different person must approve it; the server refuses self-approval. You can cancel it.</Hint>}
          <div className="flex gap-2">
            <Button disabled={own} onClick={() => onOpen('approve')}>Approve</Button>
            <Button variant="secondary" onClick={() => onOpen('reject')}>Reject</Button>
            {own && <Button variant="secondary" onClick={() => onOpen('cancel')}>Cancel request</Button>}
          </div>
        </div>
      )}
      {pending && !canAct && <p className="mt-3 text-sm text-slate-600">Your role is read only for force-resolutions.</p>}
      {!pending && <p className="mt-3 text-sm text-slate-600">This resolution is no longer pending, so it cannot be decided or cancelled.</p>}
    </section>
  )
}
