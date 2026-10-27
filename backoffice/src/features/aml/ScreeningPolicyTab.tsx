import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { AML_REASON_CODE } from '../../api/aml'
import { SCREENING_LISTS, UUID_RE, parseTriggerPolicy, readTriggerPolicy, type ScreeningList } from '../../api/amlScreening'
import {
  classifySetScreeningPolicy,
  parseScreeningPolicy,
  readScreeningPolicy,
  setScreeningPolicy,
  type SetScreeningPolicyBody,
  type SetScreeningPolicyResult,
} from '../../api/amlScreeningPolicy'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { Pending, Success } from '../licensing/LicensingBits'
import { Note, OUTCOME_UNCONFIRMED } from '../policies/PolicyBits'
import { shortId } from './AmlBits'
import { LEGAL_REF_MAX, JurisdictionInput, ScreeningError, isLegalReference } from './ScreeningBits'
import { LIST_LABEL, POLICY_SAFETY_RULES, SCREENING_POLICY_NOTE, listLabel } from './ScreeningLabels'
import { effectiveOutsideWindow, graceProblem, intervalProblem, looseningReasons, minIntervalForGate, wholeSeconds } from './screeningPolicyRules'

/**
 * The screening POLICY of a jurisdiction (lic29): lists, re-screen interval, backfill grace. Read for any officer; authoring is
 * shown only to a platform administrator with the officer flag (a UI hint: the server requires an acting AML officer grant, the
 * officer quorum, a fresh second-factor check and a signed proof). Every field is explicit: nothing is defaulted or preselected.
 */
export function ScreeningPolicyTab({ tenantId, canAuthor }: { tenantId: string; canAuthor: boolean }) {
  const qc = useQueryClient()
  const { claims } = useAuth()
  const [jur, setJur] = useState('')
  const [applied, setApplied] = useState('')
  const [authoring, setAuthoring] = useState(false)
  const [notice, setNotice] = useState<{ kind: 'ok' | 'unconfirmed'; text: string } | null>(null)
  const q = useQuery({ queryKey: ['aml', 'screening', 'screening-policy', tenantId, applied], queryFn: () => readScreeningPolicy(tenantId, applied), enabled: applied !== '', retry: false })
  // The gate alert threshold of the jurisdiction, only to show the "twice the threshold" rule with its number.
  const trig = useQuery({ queryKey: ['aml', 'screening', 'trigger-for-screening-policy', tenantId, applied], queryFn: () => readTriggerPolicy(tenantId, applied), enabled: applied !== '', retry: false })
  const policy = q.data !== undefined ? parseScreeningPolicy(q.data) : null
  const trigger = trig.data !== undefined ? parseTriggerPolicy(trig.data) : null
  const gateSeconds = trigger && trigger.gate_withdrawal ? trigger.gate_alert_after_seconds : null
  const ok = UUID_RE.test(jur)
  return (
    <section aria-label="Screening policy" className="flex flex-col gap-3">
      <Note label="Screening policy note">{SCREENING_POLICY_NOTE}</Note>
      <section aria-label="Screening policy safety rules" className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
        <p className="font-medium">Safety rules the server enforces</p>
        <ul className="mt-1 list-disc pl-5">
          {POLICY_SAFETY_RULES.map((r) => (
            <li key={r}>{r}</li>
          ))}
        </ul>
      </section>
      <form className="flex flex-wrap items-end gap-2" onSubmit={(e) => { e.preventDefault(); if (ok) { setNotice(null); setApplied(jur) } }}>
        <JurisdictionInput value={jur} onChange={setJur} selectable={getNavPermissions(claims?.role).licensingRead} />
        <Button type="submit" disabled={!ok}>Show screening policy in force</Button>
        {canAuthor && applied !== '' && <Button variant="secondary" onClick={() => { setNotice(null); setAuthoring(true) }}>Author next screening policy version</Button>}
      </form>
      {!canAuthor && <p className="text-sm text-slate-500">Authoring is not offered to your role. The screening policy is read-only here.</p>}
      {notice && (notice.kind === 'ok' ? <Success>{notice.text}</Success> : <Pending>{notice.text}</Pending>)}
      {q.isLoading && <LoadingSpinner label="Loading screening policy..." />}
      {q.error != null && <ScreeningError error={q.error} op="screening_policy_read" />}
      {q.data !== undefined && policy === null && <p role="alert" className="text-sm text-red-700">The server answered, but the screening policy could not be recognised. Nothing is shown.</p>}
      {policy && !policy.configured && (
        <p role="status" className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
          No screening policy is in force for this jurisdiction. The platform ships none: no interval and no grace are assumed.
        </p>
      )}
      {policy && policy.configured && (
        <dl aria-label="Screening policy in force" className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 rounded-md border border-border p-3 text-sm">
          <dt className="font-medium">Lists screened</dt>
          <dd>{policy.lists.length === 0 ? 'None' : policy.lists.map(listLabel).join(', ')}</dd>
          <dt className="font-medium">Re-screen interval</dt>
          <dd>{policy.rescreen_interval_seconds === null ? 'Not recognised' : `${policy.rescreen_interval_seconds} seconds`}</dd>
          <dt className="font-medium">Backfill grace</dt>
          <dd>{policy.backfill_grace_seconds === null ? 'Not recognised' : `${policy.backfill_grace_seconds} seconds`}</dd>
          <dt className="font-medium">Effective from</dt>
          <dd>{policy.effective_from && !Number.isNaN(Date.parse(policy.effective_from)) ? new Date(policy.effective_from).toLocaleString() : 'Not recognised'}</dd>
        </dl>
      )}
      {applied !== '' && (
        <p aria-label="Gate threshold of the jurisdiction" className="text-sm text-slate-700">
          {trig.isLoading ? 'Reading the withdrawal gate of this jurisdiction...' : trig.error != null ? 'The withdrawal gate of this jurisdiction could not be read; the server still enforces the interval rule.' : gateSeconds !== null ? <>A withdrawal gate is in force here with an alert threshold of {gateSeconds} seconds, so the interval must be at least {minIntervalForGate(gateSeconds)} seconds. <Badge tone="warning">Gate in force</Badge></> : 'No withdrawal gate is in force for this jurisdiction on the policy read.'}
        </p>
      )}
      {authoring && (
        <ScreeningPolicyDialog
          tenantId={tenantId}
          jurisdictionId={applied}
          inForce={policy}
          gateSeconds={gateSeconds}
          onClose={() => setAuthoring(false)}
          onDone={(r) => {
            setAuthoring(false)
            if (r.kind === 'confirmed') {
              setNotice({ kind: 'ok', text: `Screening policy version ${shortId(r.policyId)} recorded. It takes effect at its effective time; the policy in force is refreshed below.` })
              void qc.invalidateQueries({ queryKey: ['aml', 'screening', 'screening-policy'] })
            } else setNotice({ kind: 'unconfirmed', text: OUTCOME_UNCONFIRMED })
          }}
        />
      )}
    </section>
  )
}

function ScreeningPolicyDialog({
  tenantId,
  jurisdictionId,
  inForce,
  gateSeconds,
  onClose,
  onDone,
}: {
  tenantId: string
  jurisdictionId: string
  inForce: ReturnType<typeof parseScreeningPolicy>
  gateSeconds: number | null
  onClose: () => void
  onDone: (r: SetScreeningPolicyResult) => void
}) {
  const [lists, setLists] = useState<ScreeningList[]>([])
  const [intervalText, setIntervalText] = useState('')
  const [grace, setGrace] = useState('')
  const [from, setFrom] = useState('')
  const [legal, setLegal] = useState('')
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const fromDate = from === '' ? null : new Date(from)
  const fromValid = fromDate !== null && !Number.isNaN(fromDate.getTime())
  const intervalErr = intervalText === '' ? null : intervalProblem(intervalText)
  const graceErr = grace === '' ? null : graceProblem(grace)
  const intervalN = intervalProblem(intervalText) === null ? wholeSeconds(intervalText) : null
  const graceN = graceProblem(grace) === null ? wholeSeconds(grace) : null
  const belowGate = intervalN !== null && gateSeconds !== null && intervalN < minIntervalForGate(gateSeconds)
  const loosening = intervalN !== null && graceN !== null && fromValid && lists.length > 0 ? looseningReasons(inForce, { lists, interval: intervalN, grace: graceN, effectiveMs: fromDate!.getTime() }) : []
  const valid = lists.length >= 1 && intervalN !== null && graceN !== null && fromValid && isLegalReference(legal) && AML_REASON_CODE.test(reason) && loosening.length === 0
  const m = useMutation({
    mutationFn: () => {
      const body: SetScreeningPolicyBody = {
        jurisdiction_id: jurisdictionId,
        lists: SCREENING_LISTS.filter((l) => lists.includes(l)),
        rescreen_interval_seconds: intervalN as number,
        backfill_grace_seconds: graceN as number,
        effective_from: fromDate!.toISOString(),
        legal_review_reference: legal,
        reason_code: reason,
      }
      return setScreeningPolicy(tenantId, body)
    },
    onSuccess: (r) => onDone(classifySetScreeningPolicy(r)),
    onSettled: () => { sending.current = false },
  })
  return (
    <Modal title="Author the next screening policy version" onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <Note label="Screening policy note" tone="slate">{SCREENING_POLICY_NOTE} A version needs a fresh second-factor check.</Note>
        <section aria-label="Screening policy safety rules" className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
          <p className="font-medium">Safety rules the server enforces</p>
          <ul className="mt-1 list-disc pl-5">
            {POLICY_SAFETY_RULES.map((r) => (
              <li key={r}>{r}</li>
            ))}
          </ul>
        </section>
        <fieldset className="flex flex-col gap-1 text-sm">
          <legend className="font-medium text-slate-700">Lists to screen (choose at least one)</legend>
          {SCREENING_LISTS.map((l) => (
            <label key={l} className="flex items-center gap-2">
              <input type="checkbox" checked={lists.includes(l)} onChange={(ev) => setLists(ev.target.checked ? [...lists, l] : lists.filter((x) => x !== l))} />
              {LIST_LABEL[l]}
            </label>
          ))}
        </fieldset>
        <TextInput label="Re-screen interval (seconds)" value={intervalText} onChange={(e) => setIntervalText(e.target.value.trim())} inputMode="numeric" autoComplete="off" maxLength={10} help="Whole seconds, 3600 to 31536000. No default. The floor is an engineering safety limit, not a legal value." />
        {intervalErr && <p role="alert" className="text-sm text-red-700">{intervalErr}</p>}
        {belowGate && gateSeconds !== null && (
          <p role="status" className="text-sm text-amber-800">A withdrawal gate with an alert threshold of {gateSeconds} seconds is in force here, so the server will refuse an interval below {minIntervalForGate(gateSeconds)} seconds.</p>
        )}
        <TextInput label="Backfill grace (seconds)" value={grace} onChange={(e) => setGrace(e.target.value.trim())} inputMode="numeric" autoComplete="off" maxLength={10} help="Whole seconds, 0 to 31536000. No default: enter 0 only if you mean no grace." />
        {graceErr && <p role="alert" className="text-sm text-red-700">{graceErr}</p>}
        <TextInput label="Effective from" type="datetime-local" value={from} onChange={(e) => setFrom(e.target.value)} help="Your local time. The server accepts a time from now (2 minutes of clock skew) to 30 days ahead." />
        {fromValid && effectiveOutsideWindow(fromDate!.getTime()) && <p role="status" className="text-sm text-amber-800">This time looks outside the window the server accepts (now to 30 days ahead). The server decides.</p>}
        {loosening.length > 0 && (
          <div role="alert" aria-label="Loosening refused" className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-800">
            <p className="font-medium">This version is looser than the policy in force and this form would be refused. Loosening is a separate two-person request in the licensing changes area.</p>
            <ul className="mt-1 list-disc pl-5">
              {loosening.map((r) => (
                <li key={r}>{r}</li>
              ))}
            </ul>
          </div>
        )}
        <TextInput label="Legal review reference" value={legal} onChange={(e) => setLegal(e.target.value)} autoComplete="off" maxLength={LEGAL_REF_MAX} help="1 to 200 characters, no surrounding space or control characters. Points at the legal review that supports this decision. Supplied by you and not verified by the platform." />
        {legal !== '' && !isLegalReference(legal) && <p role="alert" className="text-sm text-red-700">Not a valid legal review reference.</p>}
        <TextInput label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} autoComplete="off" maxLength={64} help="Lower-case letters, digits and underscores, up to 64. A short code, never free text." />
        {reason !== '' && !AML_REASON_CODE.test(reason) && <p role="alert" className="text-sm text-red-700">Not a valid reason code.</p>}
        <ScreeningError error={m.error} op="screening_policy_set" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>Author screening policy version</Button>
        </div>
      </form>
    </Modal>
  )
}
