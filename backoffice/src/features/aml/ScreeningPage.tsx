import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { AML_REASON_CODE } from '../../api/aml'
import {
  GATE_ALERT_MAX_SECONDS,
  GATE_ALERT_MIN_SECONDS,
  SCREENING_LISTS,
  TRIGGER_EVENTS,
  UUID_RE,
  classifyResolution,
  classifySetPolicy,
  listScreeningRequests,
  parseRequests,
  parseTriggerPolicy,
  readTriggerPolicy,
  resolveScreeningRequest,
  setTriggerPolicy,
  type ResolutionOutcome,
  type ScreeningList,
  type ScreeningRequest,
  type SetTriggerPolicyBody,
  type TriggerEvent,
} from '../../api/amlScreening'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { SelectField, TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { Note, OUTCOME_UNCONFIRMED } from '../policies/PolicyBits'
import { Pending, Success } from '../licensing/LicensingBits'
import { ConfidentialityBanner, shortId, useAmlTenant } from './AmlBits'
import { JurisdictionInput, LEGAL_REF_MAX, ScreeningError, isLegalReference } from './ScreeningBits'
import { ScreeningPolicyTab } from './ScreeningPolicyTab'
import { describeScreeningError, type ScreeningOp } from './screeningErrors'
import {
  EVENT_LABEL,
  GATE_WARNING,
  TRIGGER_TIGHTEN_ONLY_NOTE,
  HOLDS_WITHDRAWALS_NOTE,
  INTERVAL_GRACE_NOTE,
  LIST_LABEL,
  NO_POLICY_SHIPPED,
  OUTCOME_LABEL,
  RESOLUTION_NOTE,
  SANCTIONS_RULE_NOTE,
  SERVER_ENFORCED_NOTE,
  eventLabel,
  isAwaitingOfficer,
  listLabel,
  requestStatus,
  safeToken,
} from './ScreeningLabels'

const SECONDS_RE = /^[1-9][0-9]{0,6}$/
// The text is already checked against SECONDS_RE (digits only, no leading zero), so JSON.parse yields an exact integer.
const secondsValue = (s: string): number => JSON.parse(s) as number
export const secondsInRange = (s: string) => {
  if (!SECONDS_RE.test(s)) return false
  const n = secondsValue(s)
  return n >= GATE_ALERT_MIN_SECONDS && n <= GATE_ALERT_MAX_SECONDS
}

type Tab = 'policy' | 'screening_policy' | 'requests'
const TAB_LABEL: Record<Tab, string> = { policy: 'Trigger policy', screening_policy: 'Screening policy', requests: 'Screening requests' }

/**
 * AML screening (lic20): the trigger policy per tenant and jurisdiction, and the officer resolution of stuck screening requests.
 * Route-gated by the `amlRead` flag as a convenience only. Authoring and resolving are shown only with the `amlHolds` flag (and
 * authoring only to platform staff); both are UI hints. The server is authoritative on every call and its refusals are shown.
 */
export function ScreeningPage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const { tenantId, picker } = useAmlTenant()
  const [tab, setTab] = useState<Tab>('policy')
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="AML Screening" description="Automatic screening trigger policy, the screening policy (re-screen interval, grace, lists) and officer resolution of screening requests." />
      <p className="text-sm">
        <Link className="text-brand-700 hover:underline" to="/aml">Back to AML cases and holds</Link>
      </p>
      <ConfidentialityBanner />
      <Note label="Server enforcement" tone="slate">{SERVER_ENFORCED_NOTE}</Note>
      {picker}
      {tenantId === '' && <p className="text-sm text-slate-500">Choose a tenant.</p>}
      {tenantId !== '' && (
        <>
          <div role="tablist" aria-label="Screening sections" className="flex gap-2 border-b border-border">
            {(['policy', 'screening_policy', 'requests'] as Tab[]).map((t) => (
              <button
                key={t}
                role="tab"
                aria-selected={tab === t}
                onClick={() => setTab(t)}
                className={`px-3 py-2 text-sm font-medium ${tab === t ? 'border-b-2 border-brand-600 text-brand-700' : 'text-slate-500 hover:text-slate-700'}`}
              >
                {TAB_LABEL[t]}
              </button>
            ))}
          </div>
          {tab === 'policy' ? (
            <PolicyTab tenantId={tenantId} canAuthor={perms.amlHolds && isPlatformAdmin(claims)} />
          ) : tab === 'screening_policy' ? (
            <ScreeningPolicyTab tenantId={tenantId} canAuthor={perms.amlHolds && isPlatformAdmin(claims)} />
          ) : (
            <RequestsTab tenantId={tenantId} canResolve={perms.amlHolds} />
          )}
        </>
      )}
    </div>
  )
}

function PolicyTab({ tenantId, canAuthor }: { tenantId: string; canAuthor: boolean }) {
  const qc = useQueryClient()
  const { claims } = useAuth()
  const [jur, setJur] = useState('')
  const [applied, setApplied] = useState('')
  const [authoring, setAuthoring] = useState(false)
  const [notice, setNotice] = useState<{ kind: 'ok' | 'unconfirmed'; text: string } | null>(null)
  const q = useQuery({ queryKey: ['aml', 'screening', 'policy', tenantId, applied], queryFn: () => readTriggerPolicy(tenantId, applied), enabled: applied !== '', retry: false })
  const policy = q.data !== undefined ? parseTriggerPolicy(q.data) : null
  const ok = UUID_RE.test(jur)
  return (
    <section aria-label="Trigger policy" className="flex flex-col gap-3">
      <Note label="No policy shipped">{NO_POLICY_SHIPPED}</Note>
      <Note label="Withdrawal gate warning">{GATE_WARNING}</Note>
      <Note label="Interval and grace" tone="slate">{INTERVAL_GRACE_NOTE}</Note>
      <form className="flex flex-wrap items-end gap-2" onSubmit={(e) => { e.preventDefault(); if (ok) { setNotice(null); setApplied(jur) } }}>
        <JurisdictionInput value={jur} onChange={setJur} selectable={getNavPermissions(claims?.role).licensingRead} />
        <Button type="submit" disabled={!ok}>Show policy in force</Button>
        {canAuthor && applied !== '' && <Button variant="secondary" onClick={() => { setNotice(null); setAuthoring(true) }}>Author next version</Button>}
      </form>
      {!canAuthor && <p className="text-sm text-slate-500">Authoring is not offered to your role. The policy is read-only here.</p>}
      {notice && (notice.kind === 'ok' ? <Success>{notice.text}</Success> : <Pending>{notice.text}</Pending>)}
      {q.isLoading && <LoadingSpinner label="Loading policy..." />}
      {q.error != null && <ScreeningError error={q.error} op="policy_read" />}
      {q.data !== undefined && policy === null && <p role="alert" className="text-sm text-red-700">The server answered, but the policy could not be recognised. Nothing is shown.</p>}
      {policy && !policy.configured && (
        <p role="status" className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
          No trigger policy is in force for this jurisdiction: nothing triggers automatically and nothing gates a withdrawal.
        </p>
      )}
      {policy && policy.configured && (
        <dl aria-label="Policy in force" className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 rounded-md border border-border p-3 text-sm">
          <dt className="font-medium">Events</dt>
          <dd>{policy.events.length === 0 ? 'None (nothing triggers)' : policy.events.map(eventLabel).join(', ')}</dd>
          <dt className="font-medium">Withdrawal gate</dt>
          <dd>{policy.gate_withdrawal ? <Badge tone="danger">ON: gates withdrawals</Badge> : <Badge>Off</Badge>}</dd>
          {policy.gate_withdrawal && (
            <>
              <dt className="font-medium">Trapped alert after</dt>
              <dd>{policy.gate_alert_after_seconds === null ? 'Not recognised' : `${policy.gate_alert_after_seconds} seconds`}</dd>
            </>
          )}
          <dt className="font-medium">Effective from</dt>
          <dd>{policy.effective_from ? new Date(policy.effective_from).toLocaleString() : 'Not recognised'}</dd>
        </dl>
      )}
      {authoring && (
        <PolicyDialog
          tenantId={tenantId}
          jurisdictionId={applied}
          onClose={() => setAuthoring(false)}
          onDone={(r) => {
            setAuthoring(false)
            if (r.kind === 'confirmed') {
              setNotice({ kind: 'ok', text: `Policy version ${shortId(r.policyId)} recorded. It takes effect at its effective time; the policy in force is refreshed below.` })
              void qc.invalidateQueries({ queryKey: ['aml', 'screening', 'policy'] })
            } else setNotice({ kind: 'unconfirmed', text: OUTCOME_UNCONFIRMED })
          }}
        />
      )}
    </section>
  )
}

function PolicyDialog({ tenantId, jurisdictionId, onClose, onDone }: { tenantId: string; jurisdictionId: string; onClose: () => void; onDone: (r: ReturnType<typeof classifySetPolicy>) => void }) {
  const [events, setEvents] = useState<TriggerEvent[]>([])
  const [gate, setGate] = useState<'' | 'off' | 'on'>('')
  const [secs, setSecs] = useState('')
  const [from, setFrom] = useState('')
  const [legal, setLegal] = useState('')
  const [reason, setReason] = useState('')
  const [ack, setAck] = useState(false)
  const sending = useRef(false)
  const fromDate = from === '' ? null : new Date(from)
  const fromValid = fromDate !== null && !Number.isNaN(fromDate.getTime())
  const now = Date.now()
  const windowWarn = fromValid && (fromDate!.getTime() < now - 2 * 60_000 || fromDate!.getTime() > now + 30 * 86_400_000)
  const gateOn = gate === 'on'
  const needScheduled = gateOn && !events.includes('scheduled')
  const valid =
    gate !== '' && fromValid && isLegalReference(legal) && AML_REASON_CODE.test(reason) && (!gateOn || (secondsInRange(secs) && !needScheduled && ack))
  const m = useMutation({
    mutationFn: () => {
      const body: SetTriggerPolicyBody = {
        jurisdiction_id: jurisdictionId,
        events: TRIGGER_EVENTS.filter((e) => events.includes(e)),
        gate_withdrawal: gateOn,
        effective_from: fromDate!.toISOString(),
        legal_review_reference: legal,
        reason_code: reason,
      }
      if (gateOn) body.gate_alert_after_seconds = secondsValue(secs)
      return setTriggerPolicy(tenantId, body)
    },
    onSuccess: (r) => onDone(classifySetPolicy(r)),
    onSettled: () => { sending.current = false },
  })
  return (
    <Modal title="Author the next trigger policy version" onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <Note label="Withdrawal gate warning">{GATE_WARNING}</Note>
        <Note label="Tighten only" tone="slate">{TRIGGER_TIGHTEN_ONLY_NOTE}</Note>
        <Note label="No policy shipped" tone="slate">{NO_POLICY_SHIPPED} A version is immutable; to change it, author another. A version needs a fresh second-factor check.</Note>
        <fieldset className="flex flex-col gap-1 text-sm">
          <legend className="font-medium text-slate-700">Events that trigger screening (choose none to trigger nothing)</legend>
          {TRIGGER_EVENTS.map((e) => (
            <label key={e} className="flex items-center gap-2">
              <input type="checkbox" checked={events.includes(e)} onChange={(ev) => setEvents(ev.target.checked ? [...events, e] : events.filter((x) => x !== e))} />
              {EVENT_LABEL[e]}
            </label>
          ))}
        </fieldset>
        <SelectField
          label="Withdrawal gate"
          value={gate}
          onChange={(e) => { setGate(e.target.value as '' | 'off' | 'on'); setSecs(''); setAck(false) }}
          options={[
            { value: '', label: 'Choose' },
            { value: 'off', label: 'Off: screening does not gate withdrawals' },
            { value: 'on', label: 'On: gate withdrawals until screening clears' },
          ]}
        />
        {gateOn && (
          <>
            {needScheduled && <p role="alert" className="text-sm text-red-700">A withdrawal gate needs the scheduled re-screen event; otherwise a clear goes stale unattended.</p>}
            <TextInput
              label="Trapped alert after (seconds)"
              value={secs}
              onChange={(e) => setSecs(e.target.value.trim())}
              inputMode="numeric"
              autoComplete="off"
              maxLength={7}
              help={`Whole seconds, ${GATE_ALERT_MIN_SECONDS} to ${GATE_ALERT_MAX_SECONDS}. After this long a held person raises the trapped alert. No default.`}
            />
            {secs !== '' && !secondsInRange(secs) && <p role="alert" className="text-sm text-red-700">Use whole seconds from 60 to 2592000.</p>}
            <label className="flex items-start gap-2 text-sm">
              <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
              <span>I understand that a withdrawal gate with no working screening provider traps player funds, and I am not enabling it in production.</span>
            </label>
          </>
        )}
        <TextInput label="Effective from" type="datetime-local" value={from} onChange={(e) => setFrom(e.target.value)} help="Your local time. The server accepts a time from now (2 minutes of clock skew) to 30 days ahead." />
        {windowWarn && <p role="status" className="text-sm text-amber-800">This time looks outside the window the server accepts (now to 30 days ahead). The server decides.</p>}
        <TextInput label="Legal review reference" value={legal} onChange={(e) => setLegal(e.target.value)} autoComplete="off" maxLength={LEGAL_REF_MAX} help="1 to 200 characters, no surrounding space or control characters. Points at the legal review that supports this decision." />
        {legal !== '' && !isLegalReference(legal) && <p role="alert" className="text-sm text-red-700">Not a valid legal review reference.</p>}
        <TextInput label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} autoComplete="off" maxLength={64} help="Lower-case letters, digits and underscores, up to 64. A short code, never free text." />
        {reason !== '' && !AML_REASON_CODE.test(reason) && <p role="alert" className="text-sm text-red-700">Not a valid reason code.</p>}
        <ScreeningError error={m.error} op="policy_set" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>Author version</Button>
        </div>
      </form>
    </Modal>
  )
}

function RequestsTab({ tenantId, canResolve }: { tenantId: string; canResolve: boolean }) {
  const qc = useQueryClient()
  const [input, setInput] = useState('')
  const [person, setPerson] = useState('')
  const [resolving, setResolving] = useState<ScreeningRequest | null>(null)
  const [notice, setNotice] = useState<{ kind: 'ok' | 'unconfirmed'; text: string } | null>(null)
  const q = useQuery({ queryKey: ['aml', 'screening', 'requests', tenantId, person], queryFn: () => listScreeningRequests(tenantId, person), enabled: person !== '', retry: false })
  const rows = q.data !== undefined ? parseRequests(q.data) : null
  const ok = UUID_RE.test(input.trim())
  const columns: Column<ScreeningRequest>[] = [
    { key: 'id', header: 'Request', render: (r) => <span className="font-mono text-xs">{shortId(r.id)}</span> },
    { key: 'trigger', header: 'Trigger', render: (r) => <span className="font-mono text-xs">{safeToken(r.trigger)}</span> },
    { key: 'status', header: 'State', render: (r) => { const s = requestStatus(r.status); return <Badge tone={s.tone}>{s.label}</Badge> } },
    { key: 'attempts', header: 'Attempts', render: (r) => r.attempts },
    { key: 'outcome', header: 'Outcome', render: (r) => (r.outcome ? <span className="font-mono text-xs">{safeToken(r.outcome)}</span> : <span className="text-slate-400">-</span>) },
    { key: 'lists', header: 'Match lists', render: (r) => (r.match_lists && r.match_lists.length > 0 ? r.match_lists.map(listLabel).join(', ') : <span className="text-slate-400">-</span>) },
    { key: 'err', header: 'Last error', render: (r) => (r.last_error_code ? <span className="font-mono text-xs">{safeToken(r.last_error_code)}</span> : <span className="text-slate-400">-</span>) },
    { key: 'resolved', header: 'Resolved', render: (r) => (r.resolved ? <Badge tone="success">Resolved</Badge> : isAwaitingOfficer(r.status, r.resolved) ? <Badge tone="warning">Holding withdrawals</Badge> : <span className="text-slate-400">No</span>) },
    { key: 'created', header: 'Created', render: (r) => new Date(r.created_at).toLocaleString() },
    {
      key: 'act',
      header: '',
      render: (r) => (canResolve && isAwaitingOfficer(r.status, r.resolved) ? <Button variant="secondary" onClick={() => { setNotice(null); setResolving(r) }}>Resolve</Button> : null),
    },
  ]
  return (
    <section aria-label="Screening requests" className="flex flex-col gap-3">
      <Note label="Holds withdrawals" tone="slate">{HOLDS_WITHDRAWALS_NOTE}</Note>
      <form className="flex flex-wrap items-end gap-2" onSubmit={(e) => { e.preventDefault(); if (ok) { setNotice(null); setPerson(input.trim()) } }}>
        <TextInput label="Person id" value={input} onChange={(e) => setInput(e.target.value)} autoComplete="off" help="Requests are listed per person (newest first, at most 200). There is no tenant-wide list." />
        <Button type="submit" disabled={!ok}>Show requests</Button>
      </form>
      {!canResolve && <p className="text-sm text-slate-500">Resolving is not offered to your role. The list is read-only here.</p>}
      {notice && (notice.kind === 'ok' ? <Success>{notice.text}</Success> : <Pending>{notice.text}</Pending>)}
      {q.isLoading && <LoadingSpinner label="Loading requests..." />}
      {q.error != null && <ScreeningError error={q.error} op="requests" />}
      {q.data !== undefined && rows === null && <p role="alert" className="text-sm text-red-700">The server answered, but the list could not be recognised. Nothing is shown.</p>}
      {rows && <Table columns={columns} rows={rows} getRowKey={(r) => r.id} emptyMessage="No screening requests for this person in your scope." />}
      {resolving && (
        <ResolveDialog
          tenantId={tenantId}
          personId={person}
          request={resolving}
          onClose={() => setResolving(null)}
          onDone={(r) => {
            setResolving(null)
            if (r.kind === 'confirmed') {
              setNotice({
                kind: 'ok',
                text: `${r.replayed ? 'Already recorded (replay): no new record was written. ' : 'Recorded. '}Result ${shortId(r.resultId)}.${r.holdId ? ` A hold was placed (${shortId(r.holdId)}).` : ''}`,
              })
            } else setNotice({ kind: 'unconfirmed', text: OUTCOME_UNCONFIRMED })
            void qc.invalidateQueries({ queryKey: ['aml', 'screening', 'requests'] })
          }}
          onStale={() => void qc.invalidateQueries({ queryKey: ['aml', 'screening', 'requests'] })}
        />
      )}
    </section>
  )
}

function ResolveDialog({ tenantId, personId, request, onClose, onDone, onStale }: { tenantId: string; personId: string; request: ScreeningRequest; onClose: () => void; onDone: (r: ReturnType<typeof classifyResolution>) => void; onStale: () => void }) {
  const [outcome, setOutcome] = useState<ResolutionOutcome | ''>('')
  const [lists, setLists] = useState<ScreeningList[]>([])
  const [matches, setMatches] = useState<ScreeningList[]>([])
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const isMatch = outcome === 'potential_match' || outcome === 'confirmed'
  const sanctionsClass = (request.match_lists ?? []).includes('sanctions')
  const valid = outcome !== '' && lists.length >= 1 && (!isMatch || matches.length >= 1) && AML_REASON_CODE.test(reason)
  const m = useMutation({
    mutationFn: () => {
      const body = { outcome: outcome as ResolutionOutcome, lists: SCREENING_LISTS.filter((l) => lists.includes(l)), reason_code: reason } as Parameters<typeof resolveScreeningRequest>[3]
      if (isMatch) body.match_lists = SCREENING_LISTS.filter((l) => matches.includes(l))
      return resolveScreeningRequest(tenantId, personId, request.id, body)
    },
    onSuccess: (r) => onDone(classifyResolution(r)),
    onError: (e) => { if (describeScreeningError(e, 'resolve').stale) onStale() },
    onSettled: () => { sending.current = false },
  })
  const toggle = (set: ScreeningList[], l: ScreeningList, on: boolean) => (on ? [...set, l] : set.filter((x) => x !== l))
  return (
    <Modal title="Resolve a screening request" onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <p className="text-sm text-slate-600">Request <span className="font-mono">{shortId(request.id)}</span>, state {requestStatus(request.status).label}.</p>
        <Note label="Resolution rules" tone="slate">{RESOLUTION_NOTE}</Note>
        <Note label="Sanctions rule">{SANCTIONS_RULE_NOTE}</Note>
        {sanctionsClass && <Note label="Sanctions-class request">This request records a sanctions-list match. A clear will be refused by the server.</Note>}
        <SelectField
          label="Outcome"
          value={outcome}
          onChange={(e) => { setOutcome(e.target.value as ResolutionOutcome | ''); setMatches([]) }}
          options={[{ value: '', label: 'Choose an outcome' }, ...Object.entries(OUTCOME_LABEL).map(([value, label]) => ({ value, label }))]}
        />
        <fieldset className="flex flex-col gap-1 text-sm">
          <legend className="font-medium text-slate-700">Lists screened</legend>
          {SCREENING_LISTS.map((l) => (
            <label key={l} className="flex items-center gap-2">
              <input type="checkbox" checked={lists.includes(l)} onChange={(e) => setLists(toggle(lists, l, e.target.checked))} />
              {LIST_LABEL[l]}
            </label>
          ))}
        </fieldset>
        {isMatch && (
          <fieldset className="flex flex-col gap-1 text-sm">
            <legend className="font-medium text-slate-700">Lists with a match</legend>
            {SCREENING_LISTS.map((l) => (
              <label key={l} className="flex items-center gap-2">
                <input type="checkbox" checked={matches.includes(l)} onChange={(e) => setMatches(toggle(matches, l, e.target.checked))} />
                {LIST_LABEL[l]}
              </label>
            ))}
          </fieldset>
        )}
        <TextInput label="Reason code" value={reason} onChange={(e) => setReason(e.target.value)} autoComplete="off" maxLength={64} help="Lower-case letters, digits and underscores, up to 64. A short code, never free text or personal data." />
        {reason !== '' && !AML_REASON_CODE.test(reason) && <p role="alert" className="text-sm text-red-700">Not a valid reason code.</p>}
        <ScreeningError error={m.error} op="resolve" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose} disabled={m.isPending}>Cancel</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>Record resolution</Button>
        </div>
      </form>
    </Modal>
  )
}
