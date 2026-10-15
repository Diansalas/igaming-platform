import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import {
  UUID_RE,
  addNote,
  approveAction,
  flagSar,
  getCase,
  proposeAction,
  transitionCase,
  type AmlCaseEvent,
  type CaseStatus,
  type ProposalKind,
} from '../../api/aml'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { SelectField, TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { AmlErrorAlert, ConfidentialityBanner, ReasonDialog, shortId } from './AmlBits'
import { describeAmlError, type AmlOp } from './errors'
import { PROPOSAL_LABEL, allowedTransitions, canFlagSar, pendingProposal, proposalKinds, statusLabel } from './rules'

type Dialog = null | { kind: 'transition'; to: CaseStatus } | { kind: 'flag' } | { kind: 'propose' } | { kind: 'approve'; proposal: AmlCaseEvent } | { kind: 'note' }

/**
 * AML case detail (ADR 0116 S6, minimal): the append-only event trail, the notes and the actions the case state permits.
 * The controls are a convenience derived from the case state and the nav flags; the server re-validates every action. Every
 * state change presents the case's last_seq (compare-and-set), so a case that moved on is refused as stale and reloaded.
 * Proposing and approving a four-eyes action ask for a fresh second factor (the step-up modal appears on the 403).
 */
export function CaseDetailPage() {
  const { caseId = '' } = useParams()
  const [search] = useSearchParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const platform = isPlatformAdmin(claims)
  const tenantId = platform ? (search.get('tenant') ?? '') : (claims?.tenant_id ?? '')
  const valid = UUID_RE.test(tenantId) && UUID_RE.test(caseId)
  const qc = useQueryClient()
  const detail = useQuery({ queryKey: ['aml', 'case', tenantId, caseId], queryFn: () => getCase(tenantId, caseId), enabled: valid, retry: false })
  const [dialog, setDialog] = useState<Dialog>(null)
  const [notice, setNotice] = useState('')

  function refresh() {
    void qc.invalidateQueries({ queryKey: ['aml'] })
  }
  function done(msg: string) {
    setDialog(null)
    setNotice(msg)
    refresh()
  }
  function failed(op: AmlOp) {
    return (e: unknown) => {
      if (describeAmlError(e, op).stale) refresh()
    }
  }

  const c = detail.data?.case
  const seq = c?.last_seq ?? 0
  const transition = useMutation({
    mutationFn: (v: { to: CaseStatus; reason: string }) => transitionCase(tenantId, caseId, seq, v.to, v.reason),
    onSuccess: () => done('Case updated.'),
    onError: failed('transition'),
  })
  const flag = useMutation({
    mutationFn: (reason: string) => flagSar(tenantId, caseId, seq, reason),
    onSuccess: () => done('Case flagged as SAR-related. This cannot be undone.'),
    onError: failed('flag_sar'),
  })
  const note = useMutation({
    mutationFn: (body: string) => addNote(tenantId, caseId, seq, body),
    onSuccess: () => done('Note added.'),
    onError: failed('note'),
  })
  const [kind, setKind] = useState<ProposalKind>('lift_hold')
  const [holdId, setHoldId] = useState('')
  const [filingRef, setFilingRef] = useState('')
  const propose = useMutation({
    mutationFn: (reason: string) =>
      proposeAction(tenantId, caseId, {
        expected_seq: seq,
        kind,
        reason_code: reason,
        ...(kind === 'lift_hold' ? { hold_id: holdId.trim() } : {}),
        ...(kind === 'close_sar_filed' ? { external_filing_reference: filingRef.trim() } : {}),
      }),
    onSuccess: () => done('Proposal recorded. A different officer must approve it, and any later event on the case withdraws it.'),
    onError: failed('propose'),
  })
  const approve = useMutation({
    mutationFn: (v: { proposal: AmlCaseEvent; reason: string }) => approveAction(tenantId, caseId, v.proposal.seq, v.proposal.content_hash ?? '', v.reason),
    onSuccess: () => done('Approved.'),
    onError: failed('approve'),
  })

  if (!valid) {
    return (
      <div className="flex flex-col gap-3">
        <PageHeader title="AML case" />
        <p className="text-sm text-slate-600">This link does not name a tenant and a case. <Link className="text-brand-700 hover:underline" to="/aml">Back to AML</Link></p>
      </div>
    )
  }

  const pending = c ? pendingProposal(c, detail.data?.events ?? []) : null
  const kinds = c ? proposalKinds(c) : []
  const proposalNeedsHold = kind === 'lift_hold'
  const proposalNeedsRef = kind === 'close_sar_filed'
  const proposalFieldsOk = (!proposalNeedsHold || UUID_RE.test(holdId.trim())) && (!proposalNeedsRef || /^[A-Za-z0-9._/-]{1,64}$/.test(filingRef.trim()))

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="AML case" description={`Case ${shortId(caseId)}`} actions={<Link className="text-sm text-brand-700 hover:underline" to="/aml">Back to AML</Link>} />
      <ConfidentialityBanner />
      {detail.isLoading && <LoadingSpinner label="Loading case..." />}
      {detail.error != null && <AmlErrorAlert error={detail.error} op="case" />}
      {notice && <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">{notice}</p>}
      {c && detail.data && (
        <>
          <section aria-label="Case summary" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
            <dl className="grid grid-cols-[11rem_1fr] gap-y-1 text-sm">
              <dt className="text-slate-500">Status</dt>
              <dd><Badge tone="info">{statusLabel(c.status)}</Badge> {c.sar_flagged && <Badge tone="danger">SAR flagged</Badge>}</dd>
              <dt className="text-slate-500">Person</dt>
              <dd className="font-mono text-xs">{c.person_id}</dd>
              <dt className="text-slate-500">Owner scope</dt>
              <dd>{c.owner_scope}</dd>
              <dt className="text-slate-500">Opened</dt>
              <dd>{new Date(c.opened_at).toLocaleString()}</dd>
              <dt className="text-slate-500">Last event</dt>
              <dd>{c.last_seq}</dd>
            </dl>
          </section>

          {perms.amlWork && (
            <section aria-label="Actions" className="flex flex-wrap items-center gap-2">
              {allowedTransitions(c).map((to) => (
                <Button key={to} variant="secondary" onClick={() => { setNotice(''); transition.reset(); setDialog({ kind: 'transition', to }) }}>
                  Move to: {statusLabel(to)}
                </Button>
              ))}
              <Button variant="secondary" onClick={() => { setNotice(''); note.reset(); setDialog({ kind: 'note' }) }}>Add a note</Button>
              {canFlagSar(c) && <Button variant="secondary" onClick={() => { setNotice(''); flag.reset(); setDialog({ kind: 'flag' }) }}>Flag as SAR-related</Button>}
              {kinds.length > 0 && (
                <Button variant="secondary" onClick={() => { setNotice(''); propose.reset(); setKind(kinds[0]); setHoldId(''); setFilingRef(''); setDialog({ kind: 'propose' }) }}>
                  Propose a four-eyes action
                </Button>
              )}
              <Button variant="ghost" onClick={refresh}>Refresh</Button>
            </section>
          )}

          {pending && perms.amlApprove && (
            <section aria-label="Pending proposal" className="rounded-lg border border-amber-300 bg-amber-50 p-4">
              <h2 className="text-sm font-semibold text-slate-900">A proposal awaits a second officer</h2>
              <p className="mt-1 text-sm text-slate-700">
                {PROPOSAL_LABEL[pending.proposal_kind as ProposalKind] ?? 'Unrecognised proposal'}
                {pending.subject_hold_id && <> (hold <span className="font-mono text-xs">{shortId(pending.subject_hold_id)}</span>)</>}
                {pending.external_filing_reference && <> (filing reference <span className="font-mono text-xs">{pending.external_filing_reference}</span>)</>}
                , reason <span className="font-mono text-xs">{pending.reason_code}</span>. Proposed by <span className="font-mono text-xs">{shortId(pending.actor_id)}</span>.
              </p>
              <p className="mt-1 text-xs text-slate-600">
                You approve exactly this proposal (content {shortId(pending.content_hash ?? '')}). You cannot approve your own proposal, one by the same person, or a case about yourself; the server decides.
              </p>
              {pending.actor_id === claims?.sub && <p className="mt-1 text-xs font-medium text-amber-900">You made this proposal, so you cannot approve it.</p>}
              <div className="mt-3">
                <Button
                  disabled={pending.actor_id === claims?.sub}
                  onClick={() => { setNotice(''); approve.reset(); setDialog({ kind: 'approve', proposal: pending }) }}
                >
                  Approve this proposal
                </Button>
              </div>
            </section>
          )}

          <section aria-label="Events" className="flex flex-col gap-2">
            <h2 className="text-sm font-semibold text-slate-900">Event trail</h2>
            <ol className="flex flex-col gap-1 text-sm">
              {detail.data.events.map((e) => (
                <li key={e.seq} className="rounded border border-border bg-surface px-3 py-2">
                  <span className="font-mono text-xs text-slate-500">#{e.seq}</span> <span className="font-medium">{e.type.replace(/_/g, ' ')}</span>
                  {e.from_status && e.to_status && <> {statusLabel(e.from_status)} to {statusLabel(e.to_status)}</>}
                  {e.proposal_kind && <> ({PROPOSAL_LABEL[e.proposal_kind] ?? 'proposal'})</>}
                  <span className="text-slate-500"> reason {e.reason_code} by {shortId(e.actor_id)} ({e.actor_scope}) {new Date(e.created_at).toLocaleString()}</span>
                </li>
              ))}
            </ol>
          </section>

          <section aria-label="Notes" className="flex flex-col gap-2">
            <h2 className="text-sm font-semibold text-slate-900">Notes</h2>
            {detail.data.notes.length === 0 && <p className="text-sm text-slate-500">No notes.</p>}
            {detail.data.notes.map((n) => (
              <article key={n.id} className="rounded border border-border bg-surface px-3 py-2 text-sm">
                <p className="whitespace-pre-wrap">{n.body}</p>
                <p className="mt-1 text-xs text-slate-500">{shortId(n.created_by)} {new Date(n.created_at).toLocaleString()}</p>
              </article>
            ))}
          </section>
        </>
      )}

      {dialog?.kind === 'transition' && (
        <ReasonDialog
          title={`Move case to: ${statusLabel(dialog.to)}`}
          confirmLabel="Move case"
          isSubmitting={transition.isPending}
          error={transition.error}
          op="transition"
          onConfirm={(reason) => transition.mutate({ to: dialog.to, reason })}
          onCancel={() => setDialog(null)}
        />
      )}
      {dialog?.kind === 'flag' && (
        <ReasonDialog
          title="Flag the case as SAR-related"
          description="Irreversible. A SAR-flagged case cannot be closed as no action. This system files nothing: a filing is a human act."
          confirmLabel="Flag case"
          isSubmitting={flag.isPending}
          error={flag.error}
          op="flag_sar"
          onConfirm={(reason) => flag.mutate(reason)}
          onCancel={() => setDialog(null)}
        />
      )}
      {dialog?.kind === 'note' && <NoteDialog isSubmitting={note.isPending} error={note.error} onConfirm={(b) => note.mutate(b)} onCancel={() => setDialog(null)} />}
      {dialog?.kind === 'propose' && (
        <ReasonDialog
          title="Propose a four-eyes action"
          description="A different officer must approve exactly this proposal. Any later event on the case withdraws it. This needs a fresh second-factor check."
          confirmLabel="Record proposal"
          isSubmitting={propose.isPending}
          error={propose.error}
          op="propose"
          extra={
            <>
              <SelectField label="Action" value={kind} onChange={(e) => setKind(e.target.value as ProposalKind)} options={kinds.map((k) => ({ value: k, label: PROPOSAL_LABEL[k] }))} />
              {proposalNeedsHold && <TextInput label="Hold id" value={holdId} onChange={(e) => setHoldId(e.target.value)} autoComplete="off" />}
              {proposalNeedsRef && (
                <TextInput label="External filing reference" value={filingRef} onChange={(e) => setFilingRef(e.target.value)} autoComplete="off" help="Entered by a human after filing outside this system. Letters, digits and . _ / - only." />
              )}
            </>
          }
          extraValid={proposalFieldsOk}
          onConfirm={(reason) => propose.mutate(reason)}
          onCancel={() => setDialog(null)}
        />
      )}
      {dialog?.kind === 'approve' && (
        <ReasonDialog
          title="Approve the proposal"
          description="Approves exactly the proposal shown. This needs a fresh second-factor check."
          confirmLabel="Approve"
          isSubmitting={approve.isPending}
          error={approve.error}
          op="approve"
          onConfirm={(reason) => approve.mutate({ proposal: dialog.proposal, reason })}
          onCancel={() => setDialog(null)}
        />
      )}
    </div>
  )
}

function NoteDialog({ isSubmitting, error, onConfirm, onCancel }: { isSubmitting: boolean; error: unknown; onConfirm: (body: string) => void; onCancel: () => void }) {
  const [body, setBody] = useState('')
  const noteId = useId()
  const ok = body.trim() !== '' && new TextEncoder().encode(body).length <= 4000
  return (
    <Modal title="Add a note" onClose={onCancel}>
      <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); if (ok && !isSubmitting) onConfirm(body) }}>
        <div className="flex flex-col gap-1 text-sm">
          <label htmlFor={noteId} className="font-medium text-slate-700">Note</label>
          <textarea
            id={noteId}
            aria-describedby={`${noteId}-help`}
            className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
            rows={5}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            autoFocus
          />
          <span id={`${noteId}-help`} className="text-xs text-slate-500">
            Do not put card numbers, IBANs, e-mail addresses, phone numbers or document numbers in a note. The server rejects them; it is a guard, not a guarantee.
          </span>
        </div>
        <AmlErrorAlert error={error} op="note" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onCancel} disabled={isSubmitting}>Cancel</Button>
          <Button type="submit" disabled={!ok} isLoading={isSubmitting}>Add note</Button>
        </div>
      </form>
    </Modal>
  )
}
