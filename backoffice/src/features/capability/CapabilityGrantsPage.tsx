import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { newIdempotencyKey } from '../../api/idempotency'
import {
  GRANT_CAPABILITIES,
  approveGrantRequest,
  cancelGrantRequest,
  createGrantRequest,
  listGrantRequests,
  listGrants,
  listTenantStaff,
  rejectGrantRequest,
  revokeGrant,
  type CapabilityGrant,
  type CapabilityGrantRequest,
  type GrantCapability,
} from '../../api/capabilityGrants'
import { getNavPermissions } from '../../auth/permissions'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Checkbox, SelectField, TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { ActingOn, ConsoleErrorAlert, Hint, Notice, PII_WARNING, actorText, isUuid, shortId, useConsoleTenant } from '../consoles/ConsoleBits'
import { refusalIsStale } from '../consoles/errors'
import { safeCode } from '../killswitch/labels'
import { CancelDialog } from '../resolutions/DecisionDialogs'

const CAP_LABEL: Record<string, string> = {
  'ledger_adjustment:initiate': 'Manual adjustment: initiate',
  'ledger_adjustment:approve': 'Manual adjustment: approve',
  'payment_force_resolve:request': 'Payment force-resolution: request',
  'payment_force_resolve:approve': 'Payment force-resolution: approve',
  'withdrawal_hold_resolution:request': 'Withdrawal hold release: request',
  'withdrawal_hold_resolution:approve': 'Withdrawal hold release: approve',
}
const capLabel = (c: unknown) => (typeof c === 'string' && Object.prototype.hasOwnProperty.call(CAP_LABEL, c) ? CAP_LABEL[c] : `Unrecognised (${safeCode(c)})`)

const REQ_STATE: Record<string, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Pending approval', tone: 'warning' },
  approved: { label: 'Approved', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'neutral' },
  cancelled: { label: 'Cancelled', tone: 'neutral' },
  expired: { label: 'Expired', tone: 'neutral' },
}
const reqState = (s: unknown) => (typeof s === 'string' && Object.prototype.hasOwnProperty.call(REQ_STATE, s) ? REQ_STATE[s] : { label: `Unrecognised (${safeCode(s)})`, tone: 'danger' as BadgeTone })

/** Display-only classification of a grant against the local clock; the database decides whether it is in force. */
export function grantStatus(g: CapabilityGrant, now: number = Date.now()): { label: string; tone: BadgeTone } {
  if (g.revoked_at) return { label: 'Revoked', tone: 'neutral' }
  if (Date.parse(g.valid_from) > now) return { label: 'Not yet valid', tone: 'info' }
  if (g.valid_until && Date.parse(g.valid_until) <= now) return { label: 'Expired', tone: 'neutral' }
  return { label: g.valid_until ? 'In force until expiry' : 'In force, no expiry', tone: 'success' }
}

/** Rows shown per page; the screen asks for one more to learn whether a next page exists. */
const K1_PAGE = 25

const when = (iso: string | undefined) => (iso && !Number.isNaN(Date.parse(iso)) ? new Date(iso).toLocaleString() : 'None')

type Dialog =
  | { op: 'request' }
  | { op: 'approve' | 'reject' | 'cancel'; req: CapabilityGrantRequest }
  | { op: 'revoke'; grant: CapabilityGrant }

/**
 * K1 capability grants console (ADR 0114 S7, ADR 0099). Lists requests and grants (bare arrays; A10c: paged with the
 * optional ?limit / ?offset window, no filters), files a request, approves / rejects (platform_admin only), cancels (the requester) and revokes. The
 * database enforces R-7 (a tenant never approves), the distinct-person rule and the no-self-grant rule; the
 * buttons here are hints and every refusal is rendered. K1 has no payload hash (approve takes only a reason), so
 * the decision dialog shows the request's frozen content instead.
 */
export function CapabilityGrantsPage() {
  const { claims, platform, tenantId, tenantLabel, picker } = useConsoleTenant()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [notice, setNotice] = useState('')
  const [reqOffset, setReqOffset] = useState(0)
  const [grantOffset, setGrantOffset] = useState(0)
  const reqs = useQuery({ queryKey: ['k1', 'requests', tenantId, reqOffset], queryFn: () => listGrantRequests(tenantId, { limit: K1_PAGE + 1, offset: reqOffset }), enabled: tenantId !== '', retry: false })
  const grants = useQuery({ queryKey: ['k1', 'grants', tenantId, grantOffset], queryFn: () => listGrants(tenantId, { limit: K1_PAGE + 1, offset: grantOffset }), enabled: tenantId !== '', retry: false })
  const reqRows = (reqs.data ?? []).slice(0, K1_PAGE)
  const reqHasNext = (reqs.data?.length ?? 0) > K1_PAGE
  const grantRows = (grants.data ?? []).slice(0, K1_PAGE)
  const grantHasNext = (grants.data?.length ?? 0) > K1_PAGE
  const refresh = () => void qc.invalidateQueries({ queryKey: ['k1'] })
  const sub = claims?.sub

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Capability grants" description="Scoped permissions that let named staff act on governed financial flows. A grant needs a request and a platform administrator's approval." />
      {picker}
      {tenantId === '' && platform && <p className="text-sm text-slate-500">Choose a tenant to see its grants.</p>}
      {tenantId !== '' && (
        <>
          <div className="flex gap-2">
            {perms.capabilityGrantRequest && <Button onClick={() => { setNotice(''); setDialog({ op: 'request' }) }}>Request a grant</Button>}
            <Button variant="secondary" onClick={refresh} isLoading={reqs.isFetching || grants.isFetching}>Refresh</Button>
          </div>
          {!perms.capabilityGrantApprove && <p className="text-sm text-slate-600">Only platform administrators approve or reject requests. Your role can read them{perms.capabilityGrantRequest ? ', file requests and revoke grants' : ''}.</p>}
          {notice && <Notice>{notice}</Notice>}

          <h2 className="text-sm font-semibold text-slate-900">Requests</h2>
          {reqs.isLoading && <LoadingSpinner label="Loading requests..." />}
          {reqs.error && <ConsoleErrorAlert error={reqs.error} family="k1" op="list" />}
          {reqs.data && reqRows.length === 0 && <p className="text-sm text-slate-600">{reqOffset > 0 ? 'No more requests.' : 'No requests for this tenant.'}</p>}
          {reqs.data && reqRows.length > 0 && (
            <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
              <table className="w-full text-left text-sm">
                <caption className="sr-only">Capability grant requests</caption>
                <thead className="bg-surface-alt text-xs uppercase tracking-wide text-slate-500">
                  <tr>{['Filed', 'Capability', 'Grantee', 'Valid until', 'Requested by', 'State', 'Request expires', 'Actions'].map((h) => <th key={h} scope="col" className="px-3 py-2 font-medium">{h}</th>)}</tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {reqRows.map((r) => {
                    const s = reqState(r.status)
                    const pending = r.status === 'pending'
                    const own = !!sub && r.requested_by === sub
                    return (
                      <tr key={r.id}>
                        <td className="px-3 py-2">{when(r.created_at)}</td>
                        <td className="px-3 py-2">{capLabel(r.capability)}</td>
                        <td className="px-3 py-2">{actorText(r.grantee_staff_id, r.grantee_scope)}</td>
                        <td className="px-3 py-2">{r.valid_until ? when(r.valid_until) : 'No expiry'}</td>
                        <td className="px-3 py-2">{actorText(r.requested_by, r.requested_by_scope)}</td>
                        <td className="px-3 py-2"><Badge tone={s.tone}>{s.label}</Badge></td>
                        <td className="px-3 py-2">{when(r.expires_at)}</td>
                        <td className="px-3 py-2">
                          <div className="flex flex-wrap gap-1">
                            {pending && perms.capabilityGrantApprove && (
                              <>
                                <Button disabled={own} title={own ? 'You filed this request; a different person must approve it.' : undefined} onClick={() => { setNotice(''); setDialog({ op: 'approve', req: r }) }} aria-label={`Approve request ${shortId(r.id)}`}>Approve</Button>
                                <Button variant="secondary" onClick={() => { setNotice(''); setDialog({ op: 'reject', req: r }) }} aria-label={`Reject request ${shortId(r.id)}`}>Reject</Button>
                              </>
                            )}
                            {pending && perms.capabilityGrantRequest && own && (
                              <Button variant="secondary" onClick={() => { setNotice(''); setDialog({ op: 'cancel', req: r }) }} aria-label={`Cancel request ${shortId(r.id)}`}>Cancel</Button>
                            )}
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}

          <Pager label="Requests" offset={reqOffset} shown={reqRows.length} hasNext={reqHasNext} onChange={setReqOffset} />

          <h2 className="text-sm font-semibold text-slate-900">Grants</h2>
          {grants.isLoading && <LoadingSpinner label="Loading grants..." />}
          {grants.error && <ConsoleErrorAlert error={grants.error} family="k1" op="list" />}
          {grants.data && grantRows.length === 0 && <p className="text-sm text-slate-600">{grantOffset > 0 ? 'No more grants.' : 'No grants for this tenant.'}</p>}
          {grants.data && grantRows.length > 0 && (
            <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
              <table className="w-full text-left text-sm">
                <caption className="sr-only">Capability grants</caption>
                <thead className="bg-surface-alt text-xs uppercase tracking-wide text-slate-500">
                  <tr>{['Granted', 'Capability', 'Grantee', 'Valid from', 'Valid until', 'Status', 'Revoke reason', 'Actions'].map((h) => <th key={h} scope="col" className="px-3 py-2 font-medium">{h}</th>)}</tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {grantRows.map((g) => {
                    const s = grantStatus(g)
                    return (
                      <tr key={g.id}>
                        <td className="px-3 py-2">{when(g.granted_at)}</td>
                        <td className="px-3 py-2">{capLabel(g.capability)}</td>
                        <td className="px-3 py-2">{actorText(g.grantee_staff_id, g.grantee_scope)}</td>
                        <td className="px-3 py-2">{when(g.valid_from)}</td>
                        <td className="px-3 py-2">{g.valid_until ? when(g.valid_until) : 'No expiry'}</td>
                        <td className="px-3 py-2"><Badge tone={s.tone}>{s.label}</Badge></td>
                        <td className="px-3 py-2 font-mono text-xs">{g.revoke_reason_code ?? '-'}</td>
                        <td className="px-3 py-2">
                          {!g.revoked_at && perms.capabilityGrantRevoke && (
                            <Button variant="danger" onClick={() => { setNotice(''); setDialog({ op: 'revoke', grant: g }) }} aria-label={`Revoke grant ${shortId(g.id)}`}>Revoke</Button>
                          )}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
          <Pager label="Grants" offset={grantOffset} shown={grantRows.length} hasNext={grantHasNext} onChange={setGrantOffset} />
          <p className="text-xs text-slate-500">The lists are paged, newest first, and have no filters. Status is computed from the local clock for display; the database decides what is in force.</p>
        </>
      )}

      {dialog?.op === 'request' && <GrantRequestDialog tenantId={tenantId} tenantLabel={tenantLabel} platform={platform} onClose={() => setDialog(null)} onRefresh={refresh} onDone={() => { setDialog(null); setNotice('Request filed. Nothing is granted until a platform administrator approves it.'); refresh() }} />}
      {(dialog?.op === 'approve' || dialog?.op === 'reject') && (
        <GrantDecisionDialog tenantId={tenantId} tenantLabel={tenantLabel} op={dialog.op} req={dialog.req} onClose={() => setDialog(null)} onRefresh={refresh} onDone={(msg) => { setDialog(null); setNotice(msg); refresh() }} />
      )}
      {dialog?.op === 'cancel' && (
        <CancelDialog title="Cancel grant request" family="k1" tenantId={tenantId} tenantLabel={tenantLabel} detail="Cancelling closes this pending request; nothing was granted. Only the person who filed it can cancel it." send={(key) => cancelGrantRequest(tenantId, dialog.req.id, key)} onClose={() => setDialog(null)} onDone={() => { setDialog(null); setNotice('Request cancelled.'); refresh() }} onRefused={refresh} />
      )}
      {dialog?.op === 'revoke' && <RevokeDialog tenantId={tenantId} tenantLabel={tenantLabel} grant={dialog.grant} onClose={() => setDialog(null)} onRefresh={refresh} onDone={() => { setDialog(null); setNotice('Grant revoked. Revoking is immediate and cannot be undone; a new request is needed to grant it again.'); refresh() }} />}
    </div>
  )
}

function Pager({ label, offset, shown, hasNext, onChange }: { label: string; offset: number; shown: number; hasNext: boolean; onChange: (n: number) => void }) {
  if (offset === 0 && !hasNext) return null
  return (
    <div className="flex items-center justify-between text-sm text-slate-600" role="navigation" aria-label={`${label} pages`}>
      <span>{shown > 0 ? `Rows ${offset + 1} to ${offset + shown}` : 'No rows on this page'}</span>
      <div className="flex gap-2">
        <Button variant="secondary" disabled={offset === 0} onClick={() => onChange(Math.max(0, offset - K1_PAGE))} aria-label={`${label}: previous page`}>Previous</Button>
        <Button variant="secondary" disabled={!hasNext} onClick={() => onChange(offset + K1_PAGE)} aria-label={`${label}: next page`}>Next</Button>
      </div>
    </div>
  )
}

function GrantRequestDialog({ tenantId, tenantLabel, platform, onClose, onDone, onRefresh }: { tenantId: string; tenantLabel: string | undefined; platform: boolean; onClose: () => void; onDone: () => void; onRefresh: () => void }) {
  const [grantee, setGrantee] = useState('')
  const [manual, setManual] = useState('')
  const [capability, setCapability] = useState<GrantCapability>('payment_force_resolve:request')
  const [until, setUntil] = useState('')
  const [reason, setReason] = useState('')
  const [ack, setAck] = useState(false)
  const [key] = useState(newIdempotencyKey)
  const [sent, setSent] = useState(0)
  const busy = useRef(false)
  const staff = useQuery({ queryKey: ['k1', 'staff', tenantId], queryFn: () => listTenantStaff(tenantId, { status: 'active' }), retry: false })
  // F1: the dropdown and the typed id are MUTUALLY EXCLUSIVE (one source, never a silent override): while one has a
  // value the other is disabled, and the request sends exactly the one that is filled.
  const granteeId = manual !== '' ? manual : grantee
  const untilIso = until ? new Date(until).toISOString() : undefined
  const m = useMutation({
    mutationFn: () => {
      setSent((n) => n + 1)
      return createGrantRequest(tenantId, { grantee_staff_id: granteeId, capability, reason_code: reason, ...(untilIso ? { valid_until: untilIso } : {}) }, key)
    },
    onSuccess: onDone,
    onError: (e) => {
      if (refusalIsStale(e, 'k1', 'request')) onRefresh()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  const untilBad = until !== '' && (Number.isNaN(Date.parse(until)) || Date.parse(until) <= Date.now())
  const valid = isUuid(granteeId) && reason.trim() !== '' && ack && !untilBad
  return (
    <Modal title="Request a capability grant" onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <ActingOn tenantId={tenantId} label={tenantLabel} />
        <p className="text-sm text-slate-700">This files a request. Nothing is granted until a platform administrator approves it, and the grantee can never be the approver of their own grant.</p>
        {platform && <Hint>A platform session cannot request a grant for a tenant staff member (the server refuses it); the tenant administrator files those. Use this form for platform staff by entering their staff id.</Hint>}
        <SelectField label="Grantee (tenant staff)" value={grantee} disabled={manual !== ''} help={manual !== '' ? 'Disabled while a staff id is typed below. Clear that field to choose from the list.' : undefined} onChange={(e) => setGrantee(e.target.value)} options={[{ value: '', label: staff.isLoading ? 'Loading staff...' : 'Choose a staff member' }, ...(staff.data?.staff ?? []).map((s) => ({ value: s.id, label: `${s.display_name ?? 'Unnamed'} - ${safeCode(s.role)} (${shortId(s.id)})` }))]} />
        {staff.error && <ConsoleErrorAlert error={staff.error} family="staff" op="list" />}
        {staff.data?.truncated && <p className="text-xs text-slate-500">The staff list was truncated by the server; enter the staff id directly if the person is not listed.</p>}
        <TextInput label="Or enter a staff id" value={manual} disabled={grantee !== ''} onChange={(e) => setManual(e.target.value.trim())} autoComplete="off" spellCheck={false} aria-invalid={manual !== '' && !isUuid(manual)} help={grantee !== '' ? 'Disabled while a staff member is selected above. Choose "Choose a staff member" to clear the selection.' : 'Use this only for a person who is not in the list (for example platform staff).'} />
        <SelectField label="Capability" value={capability} onChange={(e) => setCapability(e.target.value as GrantCapability)} options={GRANT_CAPABILITIES.map((c) => ({ value: c, label: CAP_LABEL[c] }))} />
        <TextInput label="Valid until (optional)" type="datetime-local" value={until} onChange={(e) => setUntil(e.target.value)} aria-invalid={untilBad} help={untilBad ? 'Must be a future time.' : undefined} />
        {until === '' && <Hint>No expiry: the grant stays in force until it is revoked. The server may require an expiry for some grants.</Hint>}
        <TextInput label="Reason code (required)" value={reason} onChange={(e) => setReason(e.target.value)} autoComplete="off" help={`Recorded in the audit log. ${PII_WARNING}`} />
        <Checkbox label="I understand this proposes giving a person the authority to act on money-moving flows for this tenant" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <ConsoleErrorAlert error={m.error} family="k1" op="request" retried={sent > 1} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button disabled={!valid} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>File request</Button>
        </div>
      </div>
    </Modal>
  )
}

function GrantDecisionDialog({ tenantId, tenantLabel, op, req, onClose, onDone, onRefresh }: { tenantId: string; tenantLabel: string | undefined; op: 'approve' | 'reject'; req: CapabilityGrantRequest; onClose: () => void; onDone: (msg: string) => void; onRefresh: () => void }) {
  const [reason, setReason] = useState('')
  const [ack, setAck] = useState(false)
  const [key] = useState(newIdempotencyKey)
  const [sent, setSent] = useState(0)
  const busy = useRef(false)
  // F1: who receives the authority. The directory gives the display name and role for a tenant staff id; a platform
  // grantee (or an id not listed) is shown as exactly that, never as a guess.
  const dir = useQuery({ queryKey: ['k1', 'staff-all', tenantId], queryFn: () => listTenantStaff(tenantId), retry: false })
  const person = req.grantee_staff_id ? dir.data?.staff.find((s) => s.id === req.grantee_staff_id) : undefined
  const m = useMutation({
    mutationFn: async () => {
      setSent((n) => n + 1)
      if (op === 'approve') await approveGrantRequest(tenantId, req.id, reason, key)
      else await rejectGrantRequest(tenantId, req.id, reason, key)
    },
    onSuccess: () => onDone(op === 'approve' ? 'Approved. The grant now exists and is in force from its start time until it expires or is revoked.' : 'Rejected. No grant was created.'),
    onError: (e) => {
      if (refusalIsStale(e, 'k1', op)) onRefresh()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  const valid = reason.trim() !== '' && ack
  return (
    <Modal title={op === 'approve' ? 'Approve capability grant' : 'Reject capability grant'} onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <ActingOn tenantId={tenantId} label={tenantLabel} />
        <dl className="grid grid-cols-[12rem_1fr] gap-y-1 text-sm">
          <dt className="text-slate-500">Capability</dt><dd>{capLabel(req.capability)}</dd>
          <dt className="text-slate-500">Grantee (receives the authority)</dt>
          <dd data-testid="grantee">
            <span className="break-all font-mono text-xs">{req.grantee_staff_id ?? 'Id not shown to you'}</span>
            <span className="block">
              {person
                ? `${person.display_name ?? 'Unnamed'} - ${safeCode(person.role)} (${safeCode(person.status)})`
                : dir.isLoading
                  ? 'Looking up the staff directory...'
                  : dir.error
                    ? 'The staff directory could not be read, so the name and role are unknown.'
                    : `${req.grantee_scope === 'tenant' ? 'Not in this tenant staff directory (it may be truncated)' : 'Platform staff (not listed in a tenant directory)'}`}
            </span>
          </dd>
          <dt className="text-slate-500">Valid from</dt><dd>{when(req.valid_from)}</dd>
          <dt className="text-slate-500">Valid until</dt><dd>{req.valid_until ? when(req.valid_until) : 'No expiry'}</dd>
          <dt className="text-slate-500">Requested by</dt><dd>{actorText(req.requested_by, req.requested_by_scope)}</dd>
          <dt className="text-slate-500">Reason given</dt><dd className="font-mono text-xs">{req.reason_code}</dd>
          <dt className="text-slate-500">Request id</dt><dd className="font-mono text-xs">{req.id}</dd>
        </dl>
        <p className="text-sm text-slate-700">
          {op === 'approve'
            ? 'Approving creates the grant in the same step. A grant is not a payment, but it lets this person request or approve money-moving resolutions for this tenant. The approval acts on this request id; K1 has no payload hash. The server refuses approval of a request you filed yourself.'
            : 'Rejecting closes this request. No grant is created.'}
        </p>
        <TextInput label="Reason code (required)" value={reason} onChange={(e) => setReason(e.target.value)} autoComplete="off" help={`Recorded in the audit log. ${PII_WARNING}`} />
        <Checkbox label="I reviewed the request above" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <ConsoleErrorAlert error={m.error} family="k1" op={op} retried={sent > 1} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Back</Button>
          <Button variant={op === 'approve' ? 'danger' : 'primary'} disabled={!valid} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>{op === 'approve' ? 'Approve' : 'Reject'}</Button>
        </div>
      </div>
    </Modal>
  )
}

function RevokeDialog({ tenantId, tenantLabel, grant, onClose, onDone, onRefresh }: { tenantId: string; tenantLabel: string | undefined; grant: CapabilityGrant; onClose: () => void; onDone: () => void; onRefresh: () => void }) {
  const [reason, setReason] = useState('')
  const [ack, setAck] = useState(false)
  const [key] = useState(newIdempotencyKey)
  const [sent, setSent] = useState(0)
  const busy = useRef(false)
  const m = useMutation({
    mutationFn: () => {
      setSent((n) => n + 1)
      return revokeGrant(tenantId, grant.id, reason, key)
    },
    onSuccess: onDone,
    onError: (e) => {
      if (refusalIsStale(e, 'k1', 'revoke')) onRefresh()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  return (
    <Modal title="Revoke capability grant" onClose={onClose}>
      <div className="flex flex-col gap-4">
        <ActingOn tenantId={tenantId} label={tenantLabel} />
        <p className="text-sm text-slate-700">Revoking takes effect immediately, needs no second person, and cannot be undone: a new request and approval are needed to grant it again.</p>
        <p className="text-sm">{capLabel(grant.capability)} for {actorText(grant.grantee_staff_id, grant.grantee_scope)}</p>
        <TextInput label="Reason code (required)" value={reason} onChange={(e) => setReason(e.target.value)} autoComplete="off" help={`Recorded in the audit log. ${PII_WARNING}`} />
        <Checkbox label="I understand the revocation is immediate and permanent" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <ConsoleErrorAlert error={m.error} family="k1" op="revoke" retried={sent > 1} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Back</Button>
          <Button variant="danger" disabled={reason.trim() === '' || !ack} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>Revoke grant</Button>
        </div>
      </div>
    </Modal>
  )
}
