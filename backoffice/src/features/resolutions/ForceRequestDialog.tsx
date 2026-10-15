import { useMutation, useQuery } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { newIdempotencyKey } from '../../api/idempotency'
import { BASIS_CODES, CONTEXT_CODES, FINDING_CODES, HEX64, requestForceResolution, type ConsoleKind, type ForceResolutionRequest, type PaymentForceResolution } from '../../api/forceResolutions'
import { getPaymentAttempt } from '../../api/paymentAttempts'
import { Button } from '../../components/Button'
import { Checkbox, SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { formatMinorUnits } from '../../lib/minorUnits'
import { ConsoleErrorAlert, Hint, PII_WARNING, isUuid } from '../consoles/ConsoleBits'
import { refusalIsStale } from '../consoles/errors'
import { CONSOLE_KINDS, KIND_LABELS, describeKind, describeOperation, describeReason } from '../payments/labels'

export interface RequestForm {
  attemptId: string
  kind: ConsoleKind
  findingCode: string
  basisCode: string
  contextCode: string
  evidenceHash: string
  reason: string
  note: string
}

export const EMPTY_FORM: RequestForm = { attemptId: '', kind: 'm1_deposit_evidence', findingCode: '', basisCode: '', contextCode: '', evidenceHash: '', reason: '', note: '' }

/** Pure: the exact body for a form. Only the fields the OpenAPI says the chosen kind takes are included. */
export function buildForceRequest(f: RequestForm): ForceResolutionRequest {
  const body: ForceResolutionRequest = { attempt_id: f.attemptId, kind: f.kind, reason_code: f.reason }
  if (f.kind === 'm1_deposit_evidence') {
    body.finding_code = f.findingCode
  } else {
    body.basis_code = f.basisCode
    body.evidence_ref_hash = f.evidenceHash
    if (f.contextCode) body.context_code = f.contextCode
  }
  if (f.note.trim() !== '') body.note = f.note
  return body
}

/** Pure validation mirroring the OpenAPI constraints; the server stays the authority. Returns problems, [] when sendable. */
export function validateForceRequest(f: RequestForm): string[] {
  const p: string[] = []
  if (!isUuid(f.attemptId)) p.push('Attempt id must be a UUID.')
  if (f.reason.trim() === '' || f.reason.length > 64) p.push('Reason code must be 1 to 64 characters.')
  if (f.kind === 'm1_deposit_evidence') {
    if (!(FINDING_CODES as readonly string[]).includes(f.findingCode)) p.push('Choose a finding.')
  } else {
    if (!(BASIS_CODES as readonly string[]).includes(f.basisCode)) p.push('Choose a basis.')
    if (!HEX64.test(f.evidenceHash)) p.push('Evidence reference hash must be 64 lowercase hex characters.')
  }
  if (f.note.length > 1000) p.push('Note is at most 1000 characters.')
  return p
}

export function ForceRequestDialog({ tenantId, prefillAttempt, onClose, onDone, onRefresh }: { tenantId: string; prefillAttempt: string; onClose: () => void; onDone: (r: PaymentForceResolution) => void; onRefresh: () => void }) {
  const [f, setF] = useState<RequestForm>({ ...EMPTY_FORM, attemptId: isUuid(prefillAttempt) ? prefillAttempt : '' })
  const [ack, setAck] = useState(false)
  const [typed, setTyped] = useState('')
  const [key] = useState(newIdempotencyKey)
  const busy = useRef(false)
  const [created, setCreated] = useState<PaymentForceResolution | null>(null)
  const set = <K extends keyof RequestForm>(k: K, v: RequestForm[K]) => setF((x) => ({ ...x, [k]: v }))

  const attempt = useQuery({
    queryKey: ['parked', 'detail', tenantId, f.attemptId],
    queryFn: () => getPaymentAttempt(tenantId, f.attemptId),
    enabled: isUuid(f.attemptId),
    retry: false,
  })
  const m = useMutation({
    mutationFn: () => requestForceResolution(tenantId, buildForceRequest(f), key),
    onSuccess: (r) => {
      setCreated(r)
      onRefresh()
    },
    onError: (e) => {
      if (refusalIsStale(e, 'k3', 'request')) onRefresh()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  const problems = validateForceRequest(f)
  const need = f.attemptId.slice(0, 8)
  const a = attempt.data
  const notAdmitted = a && !a.admissible_kinds.includes(f.kind)
  const valid = problems.length === 0 && ack && typed === need

  if (created) {
    return (
      <Modal title="Force-resolution proposed" onClose={() => onDone(created)}>
        <div className="flex flex-col gap-4">
          <p role="status" className="text-sm text-slate-800">
            Proposed. Nothing has executed. It needs {created.required_at_submission} approval(s) from other people and expires {new Date(created.expires_at).toLocaleString()}. The attempt and the money move only when the final required approval is recorded.
          </p>
          <div className="flex justify-end"><Button onClick={() => onDone(created)}>Close</Button></div>
        </div>
      </Modal>
    )
  }
  return (
    <Modal title="Propose a force-resolution" onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <p className="text-sm font-bold text-red-800">MOCK stack - software capability only, not a regulatory approval.</p>
        <p className="text-sm text-slate-700">
          This files a request only. Nothing executes now: the money moves only when the last required approval, from a different person, is recorded. Amount and asset are copied from the attempt by the database.
        </p>
        <TextInput label="Attempt id" value={f.attemptId} onChange={(e) => set('attemptId', e.target.value.trim())} autoComplete="off" spellCheck={false} aria-invalid={f.attemptId !== '' && !isUuid(f.attemptId)} />
        {a && (
          <dl className="grid grid-cols-[10rem_1fr] gap-y-1 rounded-md border border-border bg-surface-alt p-3 text-sm" aria-label="Attempt preview">
            <dt className="text-slate-500">Operation</dt><dd>{describeOperation(a.operation).label}</dd>
            <dt className="text-slate-500">Park reason</dt><dd>{describeReason(a.terminal_reason).label}</dd>
            <dt className="text-slate-500">Amount</dt><dd>{formatMinorUnits(a.amount_minor_units, a.exponent, a.asset_code)}</dd>
          </dl>
        )}
        {attempt.error && <ConsoleErrorAlert error={attempt.error} family="attempts" op="get" />}
        <SelectField label="Kind" value={f.kind} onChange={(e) => set('kind', e.target.value as ConsoleKind)} options={CONSOLE_KINDS.map((k) => ({ value: k, label: KIND_LABELS[k] }))} />
        {notAdmitted && <Hint>The server hint does not list {describeKind(f.kind).label} for this attempt right now. You can still file it; the database re-checks and will refuse it if it is not allowed.</Hint>}
        {f.kind === 'm1_deposit_evidence' ? (
          <SelectField label="Finding" value={f.findingCode} onChange={(e) => set('findingCode', e.target.value)} options={[{ value: '', label: 'Choose a finding' }, ...FINDING_CODES.map((c) => ({ value: c, label: c }))]} />
        ) : (
          <>
            <SelectField label="Basis" value={f.basisCode} onChange={(e) => set('basisCode', e.target.value)} options={[{ value: '', label: 'Choose a basis' }, ...BASIS_CODES.map((c) => ({ value: c, label: c }))]} />
            <SelectField label="Context (optional)" value={f.contextCode} onChange={(e) => set('contextCode', e.target.value)} options={[{ value: '', label: 'None' }, ...CONTEXT_CODES.map((c) => ({ value: c, label: c }))]} />
            <TextInput
              label="Evidence reference hash"
              value={f.evidenceHash}
              onChange={(e) => set('evidenceHash', e.target.value.trim())}
              autoComplete="off"
              spellCheck={false}
              help="64 lowercase hex characters: the SHA-256 of your external evidence reference, computed outside this console (see the operations runbook). Never type the reference itself or any personal data here."
            />
          </>
        )}
        <TextInput label="Reason code (required)" value={f.reason} onChange={(e) => set('reason', e.target.value)} help="1 to 64 characters, recorded in the audit log." autoComplete="off" />
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium text-slate-700">Note (optional)</span>
          <textarea className="rounded-md border border-border px-3 py-2 text-sm" rows={2} maxLength={1000} value={f.note} onChange={(e) => set('note', e.target.value)} />
          <span className="text-xs text-slate-500">{PII_WARNING}</span>
        </label>
        <Checkbox label="I understand this proposes a resolution of a parked payment on this tenant and that it will be audited" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <TextInput label={`Type the first 8 characters of the attempt id to confirm${need ? ` (${need})` : ''}`} value={typed} onChange={(e) => setTyped(e.target.value.trim())} autoComplete="off" spellCheck={false} />
        {problems.length > 0 && f.attemptId !== '' && <ul className="list-disc pl-5 text-xs text-slate-600" aria-label="Form problems">{problems.map((x) => <li key={x}>{x}</li>)}</ul>}
        <ConsoleErrorAlert error={m.error} family="k3" op="request" />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button variant="danger" disabled={!valid} isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>File proposal</Button>
        </div>
      </div>
    </Modal>
  )
}
