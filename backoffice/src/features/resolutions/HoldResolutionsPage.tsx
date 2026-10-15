import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { newIdempotencyKey } from '../../api/idempotency'
import { HEX64 } from '../../api/forceResolutions'
import {
  HOLD_REASON_RE,
  approveHoldResolution,
  cancelHoldResolution,
  listHoldResolutions,
  rejectHoldResolution,
  requestHoldResolution,
  type HoldResolutionRequest,
  type WithdrawalHoldResolution,
} from '../../api/holdResolutions'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Checkbox, TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { formatAmountBoth } from '../../lib/minorUnits'
import { ActingOn, ConsoleErrorAlert, Hint, MockPaymentsBanner, Notice, PII_WARNING, actorText, isUuid, shortId, useConsoleTenant } from '../consoles/ConsoleBits'
import { describeRefusalCode, refusalIsStale } from '../consoles/errors'
import { EnumText } from '../killswitch/KillSwitchBits'
import { safeCode } from '../killswitch/labels'
import { CancelDialog, DecisionDialog } from './DecisionDialogs'
import { describeResolutionState } from './ForceResolutionsPage'

export interface HoldForm {
  withdrawalId: string
  reason: string
  evidenceHash: string
  note: string
}

/** Pure: the exact body for the form (the only kind that exists is release_hold_to_player). */
export function buildHoldRequest(f: HoldForm): HoldResolutionRequest {
  const body: HoldResolutionRequest = { withdrawal_request_id: f.withdrawalId, kind: 'release_hold_to_player', reason_code: f.reason, evidence_ref_hash: f.evidenceHash }
  if (f.note.trim() !== '') body.note = f.note
  return body
}

export function validateHoldRequest(f: HoldForm): string[] {
  const p: string[] = []
  if (!isUuid(f.withdrawalId)) p.push('Withdrawal id must be a UUID.')
  if (!HOLD_REASON_RE.test(f.reason)) p.push('Reason code is a lowercase token: a letter, then letters, digits or underscores, up to 64 characters.')
  if (!HEX64.test(f.evidenceHash)) p.push('Evidence reference hash must be 64 lowercase hex characters.')
  if (f.note.length > 1000) p.push('Note is at most 1000 characters.')
  return p
}

type Dialog = { op: 'request' } | { op: 'approve' | 'reject' | 'cancel'; snap: WithdrawalHoldResolution }

const CONSEQUENCE =
  'Releasing the hold moves one APPROVED withdrawal of a tenant or brand that is not active to rejected and returns the held amount to the player\'s own cash wallet. It dispatches nothing to a provider.'

/**
 * Withdrawal hold-resolution console (ADR 0111 6.3 / ADR 0113 / ADR 0114 S8). platform_admin only; every other
 * role is refused by the route. There is no candidate list route (ADR 0114 R3), so the withdrawal to release is
 * identified by an id the operator types; this screen cannot look a withdrawal up.
 */
export function HoldResolutionsPage() {
  const { claims, platform, tenantId, tenantLabel, picker, searchParams, setSearchParams } = useConsoleTenant()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [notice, setNotice] = useState('')
  const selectedId = searchParams.get('resolution') ?? ''
  const list = useQuery({ queryKey: ['hold', 'list', tenantId], queryFn: () => listHoldResolutions(tenantId), enabled: tenantId !== '', retry: false })
  const rows = list.data?.items ?? []
  const selected = rows.find((r) => r.id === selectedId) ?? null
  const refresh = () => void qc.invalidateQueries({ queryKey: ['hold'] })
  function select(id: string) {
    const next = new URLSearchParams(searchParams)
    if (id) next.set('resolution', id)
    else next.delete('resolution')
    setSearchParams(next, { replace: true })
  }
  const pendingOwn = selected && selected.state === 'pending'
  const own = !!selected && !!claims?.sub && selected.requested_by === claims.sub

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Withdrawal hold release" description="Four-eyes release of the hold on an approved withdrawal whose tenant or brand is not active. Platform administrators only." />
      <MockPaymentsBanner />
      {picker}
      {tenantId === '' && platform && <p className="text-sm text-slate-500">Choose a tenant to see its hold-resolutions.</p>}
      {tenantId !== '' && (
        <>
          <div className="flex gap-2">
            {perms.withdrawalHoldResolutionAct && <Button onClick={() => { setNotice(''); setDialog({ op: 'request' }) }}>Request a hold release</Button>}
            <Button variant="secondary" onClick={refresh} isLoading={list.isFetching}>Refresh</Button>
          </div>
          <p className="text-xs text-slate-500">The server refuses a request while no enabled policy exists for the tenant. This screen does not show candidate withdrawals: the API has no list for them.</p>
          {notice && <Notice>{notice}</Notice>}
          {list.isLoading && <LoadingSpinner label="Loading hold-resolutions..." />}
          {list.error && <ConsoleErrorAlert error={list.error} family="hsec" op="list" />}
          {list.data && rows.length === 0 && <p className="text-sm text-slate-600">No hold-resolutions for this tenant.</p>}
          {rows.length > 0 && (
            <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
              <table className="w-full text-left text-sm">
                <caption className="sr-only">Hold-resolutions</caption>
                <thead className="bg-surface-alt text-xs uppercase tracking-wide text-slate-500">
                  <tr>{['Filed', 'Withdrawal', 'State', 'Amount', 'Requested by', 'Approvals needed', 'Expires', ''].map((h) => <th key={h || 'a'} scope="col" className="px-3 py-2 font-medium">{h}</th>)}</tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {rows.map((r) => {
                    const s = describeResolutionState(r.state)
                    return (
                      <tr key={r.id} className={r.id === selectedId ? 'bg-brand-50' : undefined}>
                        <td className="px-3 py-2">{new Date(r.created_at).toLocaleString()}</td>
                        <td className="px-3 py-2 font-mono text-xs">{shortId(r.withdrawal_request_id)}</td>
                        <td className="px-3 py-2"><Badge tone={s.tone}><EnumText view={s} /></Badge></td>
                        <td className="px-3 py-2 whitespace-nowrap">{formatAmountBoth(r.amount_minor_units, r.exponent, r.asset_code)}</td>
                        <td className="px-3 py-2">{actorText(r.requested_by, r.requested_by_scope)}</td>
                        <td className="px-3 py-2">{r.required_at_submission}</td>
                        <td className="px-3 py-2">{new Date(r.expires_at).toLocaleString()}</td>
                        <td className="px-3 py-2"><Button variant="secondary" onClick={() => select(r.id)} aria-label={`Open hold-resolution ${shortId(r.id)}`}>Details</Button></td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
          {selected && (
            <section aria-label="Hold-resolution detail" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
              <div className="flex items-start justify-between gap-2">
                <h2 className="text-sm font-semibold text-slate-900">Hold-resolution detail</h2>
                <button type="button" className="text-xs text-slate-500 hover:underline" onClick={() => select('')}>Close detail</button>
              </div>
              <dl className="mt-3 grid grid-cols-[13rem_1fr] gap-y-1 text-sm">
                <dt className="text-slate-500">Resolution id</dt><dd className="font-mono text-xs">{selected.id}</dd>
                <dt className="text-slate-500">Withdrawal</dt><dd className="font-mono text-xs">{selected.withdrawal_request_id}</dd>
                <dt className="text-slate-500">State</dt><dd><EnumText view={describeResolutionState(selected.state)} /></dd>
                {selected.refusal_code && (<><dt className="text-slate-500">Refusal</dt><dd>{describeRefusalCode(selected.refusal_code)}</dd></>)}
                <dt className="text-slate-500">Amount</dt><dd>{formatAmountBoth(selected.amount_minor_units, selected.exponent, selected.asset_code)}</dd>
                <dt className="text-slate-500">Reason code</dt><dd className="font-mono text-xs">{selected.reason_code}</dd>
                <dt className="text-slate-500">Evidence reference hash</dt><dd className="break-all font-mono text-xs">{selected.evidence_ref_hash}</dd>
                <dt className="text-slate-500">Tenant / brand status when filed</dt><dd className="font-mono text-xs">{safeCode(selected.tenant_status_at_submission)} / {safeCode(selected.brand_status_at_submission)}</dd>
                <dt className="text-slate-500">Requested by</dt><dd>{actorText(selected.requested_by, selected.requested_by_scope)}</dd>
                <dt className="text-slate-500">Approvals needed</dt><dd>{selected.required_at_submission}</dd>
                <dt className="text-slate-500">Expires</dt><dd>{new Date(selected.expires_at).toLocaleString()}</dd>
                {selected.ledger_transaction_id && (<><dt className="text-slate-500">Ledger transaction</dt><dd className="font-mono text-xs">{selected.ledger_transaction_id}</dd></>)}
                <dt className="text-slate-500">Payload hash</dt><dd className="break-all font-mono text-xs">{selected.payload_hash}</dd>
              </dl>
              {pendingOwn && perms.withdrawalHoldResolutionAct && (
                <div className="mt-4 flex flex-col gap-2">
                  {own && <Hint>You filed this request. A different platform administrator must approve it; the server refuses self-approval. You can cancel it.</Hint>}
                  <div className="flex gap-2">
                    <Button disabled={own} onClick={() => { setNotice(''); setDialog({ op: 'approve', snap: selected }) }}>Approve</Button>
                    <Button variant="secondary" onClick={() => { setNotice(''); setDialog({ op: 'reject', snap: selected }) }}>Reject</Button>
                    {own && <Button variant="secondary" onClick={() => { setNotice(''); setDialog({ op: 'cancel', snap: selected }) }}>Cancel request</Button>}
                  </div>
                </div>
              )}
              {!pendingOwn && <p className="mt-3 text-sm text-slate-600">This hold-resolution is no longer pending, so it cannot be decided or cancelled.</p>}
            </section>
          )}
        </>
      )}
      {dialog?.op === 'request' && (
        <HoldRequestDialog tenantId={tenantId} tenantLabel={tenantLabel} onClose={() => setDialog(null)} onRefresh={refresh} onDone={(r) => { setDialog(null); setNotice('Hold release requested. Nothing was released; a different platform administrator must approve.'); select(r.id) }} />
      )}
      {(dialog?.op === 'approve' || dialog?.op === 'reject') && (
        <DecisionDialog
          op={dialog.op}
          title={dialog.op === 'approve' ? 'Approve hold release' : 'Reject hold release'}
          family="hsec"
          tenantId={tenantId}
          tenantLabel={tenantLabel}
          frozenHash={dialog.snap.payload_hash}
          liveHash={rows.find((x) => x.id === dialog.snap.id)?.payload_hash}
          reasonOk={(s) => HOLD_REASON_RE.test(s)}
          reasonHelp="A lowercase token: a letter, then letters, digits or underscores, up to 64 characters."
          summary={[
            ['Withdrawal', <span key="w" className="font-mono text-xs">{dialog.snap.withdrawal_request_id}</span>],
            ['Reason code on the request', <span key="r" className="font-mono text-xs">{dialog.snap.reason_code}</span>],
            ['Evidence reference hash', <span key="e" className="break-all font-mono text-xs">{dialog.snap.evidence_ref_hash}</span>],
            ['Amount returned to the player', formatAmountBoth(dialog.snap.amount_minor_units, dialog.snap.exponent, dialog.snap.asset_code)],
            ['Requested by', actorText(dialog.snap.requested_by, dialog.snap.requested_by_scope)],
            ['Approvals needed', String(dialog.snap.required_at_submission)],
            ['Expires', new Date(dialog.snap.expires_at).toLocaleString()],
          ]}
          consequence={dialog.op === 'approve' ? `${CONSEQUENCE} If this is the last approval required it happens in the same step.` : 'Rejecting closes the request. The hold stays as it is.'}
          send={(hash, reason, key) => (dialog.op === 'approve' ? approveHoldResolution : rejectHoldResolution)(tenantId, dialog.snap.id, hash, reason, key)}
          onClose={() => setDialog(null)}
          onDone={() => setDialog(null)}
          onRefused={refresh}
        />
      )}
      {dialog?.op === 'cancel' && (
        <CancelDialog
          title="Cancel hold-release request"
          family="hsec"
          tenantId={tenantId}
          tenantLabel={tenantLabel}
          detail="Cancelling closes this pending request. The hold stays as it is. Only the person who filed it can cancel it."
          send={(key) => cancelHoldResolution(tenantId, dialog.snap.id, key)}
          onClose={() => setDialog(null)}
          onDone={() => { setDialog(null); setNotice('Hold-release request cancelled.'); refresh() }}
          onRefused={refresh}
        />
      )}
    </div>
  )
}

function HoldRequestDialog({ tenantId, tenantLabel, onClose, onDone, onRefresh }: { tenantId: string; tenantLabel: string | undefined; onClose: () => void; onDone: (r: WithdrawalHoldResolution) => void; onRefresh: () => void }) {
  const [f, setF] = useState<HoldForm>({ withdrawalId: '', reason: '', evidenceHash: '', note: '' })
  const [ack, setAck] = useState(false)
  const [typed, setTyped] = useState('')
  const [key] = useState(newIdempotencyKey)
  const [sent, setSent] = useState(0)
  const busy = useRef(false)
  const [created, setCreated] = useState<WithdrawalHoldResolution | null>(null)
  const set = <K extends keyof HoldForm>(k: K, v: HoldForm[K]) => setF((x) => ({ ...x, [k]: v }))
  const m = useMutation({
    mutationFn: () => {
      setSent((n) => n + 1)
      return requestHoldResolution(tenantId, buildHoldRequest(f), key)
    },
    onSuccess: (r) => {
      setCreated(r)
      onRefresh()
    },
    onError: (e) => {
      if (refusalIsStale(e, 'hsec', 'request')) onRefresh()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  const problems = validateHoldRequest(f)
  const need = f.withdrawalId.slice(-6)
  const valid = problems.length === 0 && ack && typed === need
  if (created) {
    return (
      <Modal title="Hold release requested" onClose={() => onDone(created)}>
        <div className="flex flex-col gap-4">
          <p role="status" className="text-sm text-slate-800">Requested. Nothing was released. It needs {created.required_at_submission} approval(s) from other platform administrators and expires {new Date(created.expires_at).toLocaleString()}.</p>
          <div className="flex justify-end"><Button onClick={() => onDone(created)}>Close</Button></div>
        </div>
      </Modal>
    )
  }
  return (
    <Modal title="Request a hold release" onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <p className="text-sm font-bold text-red-800">MOCK stack - software capability only, not a regulatory approval.</p>
        <ActingOn tenantId={tenantId} label={tenantLabel} />
        <p className="text-sm text-slate-700">{CONSEQUENCE} This only files a request; nothing is released until the required approvals are recorded.</p>
        <TextInput label="Withdrawal id" value={f.withdrawalId} onChange={(e) => set('withdrawalId', e.target.value.trim())} autoComplete="off" spellCheck={false} aria-invalid={f.withdrawalId !== '' && !isUuid(f.withdrawalId)} help="The API has no candidate list; enter the id of the approved withdrawal." />
        <TextInput label="Reason code (required)" value={f.reason} onChange={(e) => set('reason', e.target.value)} autoComplete="off" help="Lowercase token, for example tenant_closed." />
        <TextInput label="Evidence reference hash" value={f.evidenceHash} onChange={(e) => set('evidenceHash', e.target.value.trim())} autoComplete="off" spellCheck={false} help="64 lowercase hex characters: the SHA-256 of your external evidence reference, computed outside this console. Never type the reference itself or personal data." />
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium text-slate-700">Note (optional)</span>
          <textarea className="rounded-md border border-border px-3 py-2 text-sm" rows={2} maxLength={1000} value={f.note} onChange={(e) => set('note', e.target.value)} />
          <span className="text-xs text-slate-500">{PII_WARNING}</span>
        </label>
        <Checkbox label="I understand this proposes returning a held amount to a player and that it will be audited" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <TextInput label="Type the LAST 6 characters of the withdrawal id above to confirm" value={typed} onChange={(e) => setTyped(e.target.value.trim())} autoComplete="off" spellCheck={false} />
        {problems.length > 0 && f.withdrawalId !== '' && <ul className="list-disc pl-5 text-xs text-slate-600" aria-label="Form problems">{problems.map((x) => <li key={x}>{x}</li>)}</ul>}
        <ConsoleErrorAlert error={m.error} family="hsec" op="request" retried={sent > 1} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button variant="danger" disabled={!valid} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>File request</Button>
        </div>
      </div>
    </Modal>
  )
}
