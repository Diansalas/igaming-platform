import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { Link, Navigate, useNavigate, useParams } from 'react-router-dom'
import { isReasonCode } from '../../api/licensing'
import { getTenant } from '../../api/tenants'
import {
  WRP_ACTIONS,
  actionRank,
  approveWithdrawalRestrictionChange,
  cancelWithdrawalRestrictionChange,
  classifyWrpDecision,
  classifyWrpFiling,
  fileWithdrawalRestrictionChange,
  getWithdrawalRestrictionChange,
  getWithdrawalRestrictionPolicy,
  listWithdrawalRestrictionChanges,
  listWithdrawalRestrictionVersions,
  rejectWithdrawalRestrictionChange,
  type WrpAction,
  type WrpChange,
  type WrpChangeStatus,
  type WrpConfigurableCause,
  type WrpCause,
  type WrpFilingResult,
  type WrpVersion,
} from '../../api/withdrawalRestriction'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { SelectField, TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { TenantPicker } from '../licensing/FileChangeDialog'
import { Pending, Success, short } from '../licensing/LicensingBits'
import { when } from '../licensing/labels'
import {
  ACTION_MEANING,
  HARD_RESTRICTIONS_NOTE,
  NO_VALUE_FROM_PLATFORM,
  WRP_APPROVAL_RULES,
  WRP_SEMANTICS_NOTE,
  WRP_STATUS_NOTE,
  WRP_UNCONFIGURED_NOTE,
  actionLabel,
  causeLabel,
  safeToken,
  stageLabel,
  wrpStatus,
} from './labels'
import { Note, OUTCOME_UNCONFIRMED, PolicyErrorAlert, ReasonCodeField } from './PolicyBits'
import type { PolicyCtx } from './errors'

const tenantPath = (id: string) => `/withdrawal-restrictions/tenants/${encodeURIComponent(id)}`
const changePath = (id: string) => `/withdrawal-restrictions/changes/${encodeURIComponent(id)}`
const CONFIGURABLE: readonly WrpConfigurableCause[] = ['licence_suspended', 'licence_expired', 'licence_not_yet_issued', 'licence_missing']
const NOTE_MAX = 1000

function ActionBadge({ action }: { action: string }) {
  const a = actionLabel(action)
  return <Badge tone={a.tone}>{a.label}</Badge>
}

/** Landing: a platform administrator chooses a tenant explicitly; a tenant role goes straight to its own tenant. */
export function WithdrawalRestrictionsHome() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const nav = useNavigate()
  const [tenant, setTenant] = useState('')
  if (!perms.wrpChange && claims?.tenant_id) return <Navigate to={tenantPath(claims.tenant_id)} replace />
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Withdrawal restriction policy" description="What happens to a withdrawal when a tenant's licence is suspended, expired, not yet issued or missing." />
      <Note label="Source of values">{NO_VALUE_FROM_PLATFORM}</Note>
      <Card title="Tenant">
        <p className="mb-3 text-sm text-slate-700">Choose a tenant to see its effective policy. No tenant is selected for you.</p>
        <TenantPicker value={tenant} onChange={(id) => { setTenant(id); if (id) nav(tenantPath(id)) }} />
      </Card>
      {perms.wrpChange && (
        <p className="text-sm">
          <Link className="text-brand-700 hover:underline" to="/withdrawal-restrictions/changes">All policy change requests</Link>
        </p>
      )}
    </div>
  )
}

function FileChangeDialog({ tenantId, causes, effective, onClose, onDone }: { tenantId: string; causes: WrpCause[]; effective: Record<string, WrpAction | undefined>; onClose: () => void; onDone: (r: WrpFilingResult) => void }) {
  const [cause, setCause] = useState<WrpConfigurableCause | ''>('')
  const [action, setAction] = useState<WrpAction | ''>('')
  const [reason, setReason] = useState('')
  const [note, setNote] = useState('')
  const sending = useRef(false)
  const m = useMutation({
    mutationFn: () => {
      const body = { tenant_id: tenantId, cause: cause as WrpConfigurableCause, action: action as WrpAction, reason_code: reason, ...(note.trim() !== '' ? { note: note.trim() } : {}) }
      return fileWithdrawalRestrictionChange(body)
    },
    onSuccess: (r) => onDone(classifyWrpFiling(r)),
    onSettled: () => {
      sending.current = false
    },
  })
  const valid = cause !== '' && action !== '' && isReasonCode(reason) && note.length <= NOTE_MAX
  const current = cause !== '' ? effective[cause] : undefined
  const hint = cause === '' || action === '' || !current ? null : actionRank(action) >= actionRank(current) ? 'tightening' : 'loosening'
  const offered = causes.filter((c) => c.configurable && (CONFIGURABLE as readonly string[]).includes(c.cause))
  return (
    <Modal title="File a withdrawal restriction change" onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <Note label="Source of values">{NO_VALUE_FROM_PLATFORM}</Note>
        <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">Tenant: <span className="font-mono text-xs">{tenantId}</span></p>
        <SelectField
          label="Cause"
          value={cause}
          onChange={(e) => setCause(e.target.value as WrpConfigurableCause | '')}
          options={[{ value: '', label: 'Choose a cause' }, ...offered.map((c) => ({ value: c.cause, label: causeLabel(c.cause) }))]}
          help="Only licence-state causes can be configured. Tenant and brand status cannot."
        />
        <SelectField
          label="Action"
          value={action}
          onChange={(e) => setAction(e.target.value as WrpAction | '')}
          options={[{ value: '', label: 'Choose an action' }, ...WRP_ACTIONS.map((a) => ({ value: a, label: actionLabel(a).label }))]}
          help={action !== '' ? ACTION_MEANING[action] : 'The action applies at the request, dispatch and re-dispatch stages.'}
        />
        {current && cause !== '' && <p className="text-sm text-slate-700">Action in force for this cause (strictest stage): {actionLabel(current).label}.</p>}
        {hint === 'tightening' && <Note label="Class hint" tone="slate">Expected: a tightening or equal change. It would apply at once with one platform administrator. The database makes the final classification.</Note>}
        {hint === 'loosening' && <Note label="Class hint" tone="slate">Expected: a LOOSENING change. It would be filed pending and take effect only after 2 approvals by 2 different people, neither the requester. The database makes the final classification.</Note>}
        <ReasonCodeField value={reason} onChange={setReason} />
        <div className="flex flex-col gap-1 text-sm">
          <label htmlFor="wrp-note" className="font-medium text-slate-700">Note (optional)</label>
          <textarea id="wrp-note" className="rounded-md border border-border px-3 py-2 text-sm" rows={3} maxLength={NOTE_MAX} value={note} onChange={(e) => setNote(e.target.value)} />
          <span className="text-xs text-slate-500">Free text for the record, at most 1000 characters. Do not enter personal data.</span>
        </div>
        <Note label="Semantics" tone="slate">{WRP_SEMANTICS_NOTE}</Note>
        {m.error && <PolicyErrorAlert error={m.error} ctx="wrp_file" />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>File change</Button>
        </div>
      </form>
    </Modal>
  )
}

function FilingOutcome({ result }: { result: WrpFilingResult }) {
  if (result.kind === 'applied') {
    const c = result.response.change
    return (
      <Success>
        Applied at once. {causeLabel(c.cause)} is now "{actionLabel(c.action).label}" (a tightening or equal change, so no second approval was needed).{' '}
        <Link className="font-medium underline" to={changePath(c.id)}>Open record</Link>
      </Success>
    )
  }
  if (result.kind === 'pending') {
    const c = result.response.change
    return (
      <Pending>
        Pending approval. Nothing has changed yet. This is a loosening change: it needs {c.required_approvals} approvals from different people before it takes effect.{' '}
        <Link className="font-medium underline" to={changePath(c.id)}>Open request {safeToken(c.id).slice(0, 8)}</Link>
      </Pending>
    )
  }
  return <Pending>{OUTCOME_UNCONFIRMED}</Pending>
}

export function WithdrawalRestrictionTenantPage() {
  const { tenantId = '' } = useParams()
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [dialog, setDialog] = useState(false)
  const [outcome, setOutcome] = useState<WrpFilingResult | null>(null)
  const policy = useQuery({ queryKey: ['wrp', 'policy', tenantId], queryFn: () => getWithdrawalRestrictionPolicy(tenantId), enabled: !!tenantId, retry: false })
  const versions = useQuery({ queryKey: ['wrp', 'versions', tenantId], queryFn: () => listWithdrawalRestrictionVersions(tenantId), enabled: !!tenantId, retry: false })
  const open = useQuery({
    queryKey: ['wrp', 'open', tenantId],
    queryFn: () => listWithdrawalRestrictionChanges({ tenant_id: tenantId, status: 'requested', limit: 50 }),
    enabled: !!tenantId && perms.wrpChange,
    retry: false,
  })
  const tenant = useQuery({ queryKey: ['wrp', 'tenant', tenantId], queryFn: () => getTenant(tenantId), enabled: !!tenantId && perms.wrpChange, retry: false })

  if (policy.isLoading) return <LoadingSpinner label="Loading withdrawal restriction policy..." />
  const p = policy.data
  const configurable = (p?.causes ?? []).filter((c) => c.configurable)
  const info = (p?.causes ?? []).filter((c) => !c.configurable)
  const strictest: Record<string, WrpAction | undefined> = {}
  for (const c of configurable) {
    let best: WrpAction | undefined
    for (const s of c.stages) if (!best || actionRank(s.action) > actionRank(best)) best = s.action
    strictest[c.cause] = best
  }

  const versionCols: Column<WrpVersion>[] = [
    { key: 'v', header: 'Set version', render: (v) => v.set_version },
    { key: 'c', header: 'Cause', render: (v) => causeLabel(v.cause) },
    { key: 'a', header: 'Action', render: (v) => <ActionBadge action={v.action} /> },
    { key: 'r', header: 'Reason code', render: (v) => <span className="font-mono text-xs">{safeToken(v.reason_code)}</span> },
    { key: 'ch', header: 'Change', render: (v) => (v.change_id && perms.wrpChange ? <Link className="text-brand-700 hover:underline" to={changePath(v.change_id)}>{short(v.change_id)}</Link> : v.change_id ? short(v.change_id) : 'None') },
    { key: 'by', header: 'By', render: (v) => (v.created_by ? short(v.created_by) : 'Not shown') },
    { key: 'at', header: 'Recorded', render: (v) => when(v.created_at) },
  ]

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={`Withdrawal restriction policy${tenant.data ? ` of ${tenant.data.name}` : ''}`}
        description="The action taken on a withdrawal, per licence cause and stage."
        actions={perms.wrpChange && p ? <Button onClick={() => { setOutcome(null); setDialog(true) }}>File a change</Button> : undefined}
      />
      <p className="text-xs text-slate-600">Tenant <span className="font-mono">{tenantId}</span>{p ? ` - configured set version ${p.set_version}` : ''}</p>
      {policy.error ? <PolicyErrorAlert error={policy.error} ctx="read" /> : null}
      {p && (
        <>
          <Note label="Disclaimer">{safeText(p.disclaimer)}</Note>
          <Note label="Hard restrictions" tone="slate">{HARD_RESTRICTIONS_NOTE}</Note>
          {!p.configured && <Note label="Unconfigured">{WRP_UNCONFIGURED_NOTE}</Note>}
          {outcome && <FilingOutcome result={outcome} />}
          {configurable.map((c) => (
            <Card key={c.cause} title={causeLabel(c.cause)}>
              {c.unconfigured && <Note label={`Unconfigured ${safeToken(c.cause)}`}>{WRP_UNCONFIGURED_NOTE}</Note>}
              {c.unconfigured && c.notice ? <p className="mt-2 text-xs text-slate-600">Server notice: {c.notice}</p> : null}
              <div className="mt-2">
                <Table
                  columns={[
                    { key: 's', header: 'Stage', render: (s) => stageLabel(s.stage) },
                    { key: 'a', header: 'Effective action', render: (s) => <ActionBadge action={s.action} /> },
                    { key: 'src', header: 'Source', render: (s) => (s.source === 'configured' ? <Badge tone="info">Configured (set version {s.set_version ?? '?'})</Badge> : <Badge tone="warning">Default, unconfigured</Badge>) },
                  ]}
                  rows={c.stages}
                  getRowKey={(s) => s.stage}
                />
              </div>
              <p className="mt-2 text-xs text-slate-600">{c.current_behaviour}</p>
              <p className="mt-1 text-xs text-slate-500">Reference: {c.governing_reference}</p>
            </Card>
          ))}
          <Card title="Not configurable here (information only)">
            <Note label="Status causes" tone="slate">{WRP_STATUS_NOTE}</Note>
            <ul className="mt-3 flex flex-col gap-2 text-sm">
              {info.map((c) => (
                <li key={c.cause} className="rounded-md border border-border px-3 py-2">
                  <span className="font-medium">{causeLabel(c.cause)}</span> <Badge tone="neutral">{c.evaluated ? 'Fixed by owner decision' : 'Not evaluated per withdrawal'}</Badge>
                  <p className="mt-1 text-xs text-slate-600">{c.current_behaviour}</p>
                  <p className="mt-1 text-xs text-slate-500">Reference: {c.governing_reference}</p>
                </li>
              ))}
            </ul>
          </Card>
          <Note label="Semantics" tone="slate">{WRP_SEMANTICS_NOTE}</Note>
        </>
      )}
      {perms.wrpChange && (
        <Card title="Open change requests for this tenant">
          {open.error ? <PolicyErrorAlert error={open.error} ctx="read" /> : null}
          <Table
            columns={changeColumns(claims?.sub)}
            rows={open.data?.items ?? []}
            getRowKey={(c) => c.id}
            isLoading={open.isLoading}
            emptyMessage="No change request is open for this tenant."
          />
          <p className="mt-2 text-sm"><Link className="text-brand-700 hover:underline" to="/withdrawal-restrictions/changes">All change requests</Link></p>
        </Card>
      )}
      <Card title="Version history (append-only)">
        {versions.error ? <PolicyErrorAlert error={versions.error} ctx="read" /> : null}
        <Table columns={versionCols} rows={versions.data?.items ?? []} getRowKey={(v) => v.id} isLoading={versions.isLoading} emptyMessage="No policy has ever been recorded for this tenant." />
      </Card>
      {!perms.wrpChange && <p className="text-sm text-slate-600">Your role can read this policy. Only platform administrators can file or decide changes.</p>}
      {perms.wrpChange && <Link to="/withdrawal-restrictions" className="text-sm text-brand-700 hover:underline">&larr; Choose another tenant</Link>}
      {dialog && p && (
        <FileChangeDialog
          tenantId={tenantId}
          causes={p.causes}
          effective={strictest}
          onClose={() => setDialog(false)}
          onDone={(r) => {
            setDialog(false)
            setOutcome(r)
            void qc.invalidateQueries({ queryKey: ['wrp'] })
          }}
        />
      )}
    </div>
  )
}

/** The server disclaimer is a one-value enum; it is rendered as React text only. */
function safeText(s: unknown): string {
  return typeof s === 'string' ? s : ''
}

function changeColumns(sub: string | undefined): Column<WrpChange>[] {
  return [
    { key: 'id', header: 'Request', render: (c) => <Link className="font-medium text-brand-700 hover:underline" to={changePath(c.id)}>{short(c.id)}</Link> },
    { key: 'cause', header: 'Cause', render: (c) => causeLabel(c.cause) },
    { key: 'action', header: 'Requested action', render: (c) => <ActionBadge action={c.action} /> },
    { key: 'class', header: 'Class (database)', render: (c) => (c.widening ? 'Loosening' : 'Tightening or equal') },
    { key: 'st', header: 'Status', render: (c) => { const s = wrpStatus(c.status); return <Badge tone={s.tone}>{s.label}</Badge> } },
    { key: 'ap', header: 'Approvals', render: (c) => `${c.approvals.length} of ${c.required_approvals}` },
    { key: 'by', header: 'Requested by', render: (c) => (c.requested_by ? `${short(c.requested_by)}${sub === c.requested_by ? ' (you)' : ''}` : 'Not shown') },
    { key: 'at', header: 'Filed', render: (c) => when(c.created_at) },
  ]
}

const STATUS_FILTERS: WrpChangeStatus[] = ['requested', 'approved', 'executed', 'rejected', 'cancelled', 'expired']

export function WithdrawalRestrictionChangesPage() {
  const { claims } = useAuth()
  const [status, setStatus] = useState<WrpChangeStatus | ''>('')
  const q = useQuery({
    queryKey: ['wrp', 'changes', status],
    queryFn: () => listWithdrawalRestrictionChanges({ status: status || undefined, limit: 200 }),
    retry: false,
  })
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Withdrawal restriction policy changes" description="Requests to change a tenant's withdrawal restriction policy." />
      <Note label="Approval rules" tone="slate">{WRP_SEMANTICS_NOTE}</Note>
      <Card title="Filter">
        <SelectField
          label="Status"
          value={status}
          onChange={(e) => setStatus(e.target.value as WrpChangeStatus | '')}
          options={[{ value: '', label: 'Any status' }, ...STATUS_FILTERS.map((s) => ({ value: s, label: wrpStatus(s).label }))]}
        />
      </Card>
      {q.error ? <PolicyErrorAlert error={q.error} ctx="read" /> : null}
      <Table columns={changeColumns(claims?.sub)} rows={q.data?.items ?? []} getRowKey={(c) => c.id} isLoading={q.isLoading} emptyMessage="No withdrawal restriction change request matches." />
      {(q.data?.items.length ?? 0) >= 200 && <p className="text-xs text-slate-600">The newest 200 matching requests are shown.</p>}
      <Link to="/withdrawal-restrictions" className="text-sm text-brand-700 hover:underline">&larr; Back to withdrawal restrictions</Link>
    </div>
  )
}

type Op = 'approve' | 'reject' | 'cancel'

function DecisionDialog({ op, change, onClose, onDone }: { op: Op; change: WrpChange; onClose: () => void; onDone: (body: unknown) => void }) {
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const needsReason = op !== 'cancel'
  const valid = !needsReason || isReasonCode(reason)
  const m = useMutation({
    mutationFn: () => {
      if (op === 'approve') return approveWithdrawalRestrictionChange(change.id, reason)
      if (op === 'reject') return rejectWithdrawalRestrictionChange(change.id, reason)
      return cancelWithdrawalRestrictionChange(change.id)
    },
    onSuccess: onDone,
    onSettled: () => {
      sending.current = false
    },
  })
  const ctx: PolicyCtx = op === 'approve' ? 'wrp_approve' : op === 'reject' ? 'wrp_reject' : 'wrp_cancel'
  const title = op === 'approve' ? 'Approve withdrawal restriction change' : op === 'reject' ? 'Reject withdrawal restriction change' : 'Cancel withdrawal restriction change'
  return (
    <Modal title={title} onClose={onClose}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <p className="text-sm">{causeLabel(change.cause)} to {actionLabel(change.action).label} - request <span className="font-mono text-xs">{short(change.id)}</span></p>
        {op === 'approve' && <p className="text-sm text-slate-700">Check the content of the request against the change ticket, not against memory. {change.required_approvals > 1 ? `This change needs ${change.required_approvals} approvals from different people; the final approval executes it.` : 'The server executes the change once enough approvals exist.'} Approving needs a fresh second-factor check.</p>}
        {op === 'reject' && <p className="text-sm text-slate-700">Rejecting is final for this request. No second-factor check is needed.</p>}
        {op === 'cancel' && <p className="text-sm text-slate-700">Only the person who filed the request can cancel it. Cancelling is final. No reason code is taken and no second-factor check is needed.</p>}
        {needsReason && <ReasonCodeField value={reason} onChange={setReason} />}
        {m.error && <PolicyErrorAlert error={m.error} ctx={ctx} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" variant={op === 'approve' ? 'primary' : 'danger'} disabled={!valid} isLoading={m.isPending}>{op === 'approve' ? 'Approve' : op === 'reject' ? 'Reject' : 'Cancel request'}</Button>
        </div>
      </form>
    </Modal>
  )
}

export function WithdrawalRestrictionChangeDetailPage() {
  const { changeId = '' } = useParams()
  const { claims } = useAuth()
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<Op | null>(null)
  const [outcome, setOutcome] = useState<{ text: string; ok: boolean } | null>(null)
  const q = useQuery({ queryKey: ['wrp', 'change', changeId], queryFn: () => getWithdrawalRestrictionChange(changeId), enabled: !!changeId, retry: false })
  if (q.isLoading) return <LoadingSpinner label="Loading request..." />
  if (q.error) return <PolicyErrorAlert error={q.error} ctx="read" />
  const c = q.data?.change
  if (!c) return null
  const sub = claims?.sub
  const own = !!sub && c.requested_by === sub
  const mine = !!sub && c.approvals.some((a) => a.approver_id === sub)
  const isOpen = c.status === 'requested'
  const expired = Date.parse(c.expires_at) <= Date.now()
  const st = wrpStatus(c.status)

  function done(op: Op, body: unknown) {
    setDialog(null)
    const r = classifyWrpDecision(op, body)
    if (r.kind === 'executed') setOutcome({ ok: true, text: 'Approval recorded and the change was executed.' })
    else if (r.kind === 'recorded') setOutcome({ ok: false, text: `Approval recorded: ${r.response.change.approvals.length} of ${r.response.change.required_approvals}. Still pending approval - the change has not taken effect.` })
    else if (r.kind === 'rejected') setOutcome({ ok: true, text: 'Rejected. The request is closed and nothing was changed.' })
    else if (r.kind === 'cancelled') setOutcome({ ok: true, text: 'Cancelled. The request is closed and nothing was changed.' })
    else setOutcome({ ok: false, text: OUTCOME_UNCONFIRMED })
    void qc.invalidateQueries({ queryKey: ['wrp'] })
  }

  return (
    <div className="flex flex-col gap-4">
      <Card title={`${causeLabel(c.cause)} to ${actionLabel(c.action).label} - request ${short(c.id)}`}>
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div><dt className="text-slate-500">Status</dt><dd className="mt-1"><Badge tone={st.tone}>{st.label}</Badge></dd></div>
          <div><dt className="text-slate-500">Class (computed by the database)</dt><dd className="mt-1">{c.widening ? 'Loosening' : 'Tightening or equal'}</dd></div>
          <div><dt className="text-slate-500">Approvals</dt><dd className="mt-1">{c.approvals.length} of {c.required_approvals} required</dd></div>
          <div><dt className="text-slate-500">Request expires</dt><dd className="mt-1">{when(c.expires_at)}{isOpen && expired ? ' (expired)' : ''}</dd></div>
          <div><dt className="text-slate-500">Tenant</dt><dd className="mt-1"><Link className="text-brand-700 hover:underline" to={tenantPath(c.tenant_id)}>{short(c.tenant_id)}</Link></dd></div>
          <div><dt className="text-slate-500">Filed</dt><dd className="mt-1">{when(c.created_at)}</dd></div>
          <div><dt className="text-slate-500">Requested by</dt><dd className="mt-1">{c.requested_by ? `Platform staff (${short(c.requested_by)})${own ? ' - you' : ''}` : 'Not shown'}</dd></div>
          <div><dt className="text-slate-500">Reason code</dt><dd className="mt-1 font-mono text-xs">{safeToken(c.reason_code)}</dd></div>
          <div><dt className="text-slate-500">Based on set version</dt><dd className="mt-1">{c.base_set_version}</dd></div>
          <div><dt className="text-slate-500">Payload hash</dt><dd className="mt-1 break-all font-mono text-xs">{safeToken(c.payload_hash)}</dd></div>
        </dl>
        {c.note ? <p className="mt-3 whitespace-pre-wrap text-sm text-slate-700">Note: {c.note}</p> : null}
        {c.status === 'executed' && <p className="mt-3 text-sm">Executed {when(c.executed_at)}.</p>}
        {c.decision_reason_code && <p className="mt-3 text-sm">Decision reason code: <span className="font-mono text-xs">{safeToken(c.decision_reason_code)}</span></p>}
      </Card>
      <Card title="Approvals">
        {c.approvals.length === 0 ? (
          <p className="text-sm text-slate-600">No approvals yet.</p>
        ) : (
          <ul className="flex flex-col gap-1 text-sm">
            {c.approvals.map((a, i) => (
              <li key={`${a.decided_at}-${i}`}>
                {a.approver_id ? `Platform staff (${short(a.approver_id)})${sub && a.approver_id === sub ? ' - you' : ''}` : 'Platform staff'} - {when(a.decided_at)} - reason <span className="font-mono text-xs">{safeToken(a.reason_code)}</span>
              </li>
            ))}
          </ul>
        )}
      </Card>
      {c.widening && (
        <section aria-label="Approval rules" className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
          <p className="font-medium">How approval works</p>
          <ul className="mt-1 list-disc pl-5">{WRP_APPROVAL_RULES.map((r) => <li key={r}>{r}</li>)}</ul>
        </section>
      )}
      <Note label="Hard restrictions" tone="slate">{HARD_RESTRICTIONS_NOTE}</Note>
      {outcome && (outcome.ok ? <Success>{outcome.text}</Success> : <Pending>{outcome.text}</Pending>)}
      {isOpen && (
        <div className="flex flex-wrap gap-2" aria-label="Decisions">
          <Button disabled={own || mine || expired} title={own ? 'You filed this request; a different person must approve it.' : mine ? 'You already approved this request.' : undefined} onClick={() => { setOutcome(null); setDialog('approve') }}>Approve</Button>
          <Button variant="secondary" disabled={own || expired} title={own ? 'You filed this request; a different person must decide it.' : undefined} onClick={() => { setOutcome(null); setDialog('reject') }}>Reject</Button>
          {own && <Button variant="secondary" onClick={() => { setOutcome(null); setDialog('cancel') }}>Cancel request</Button>}
        </div>
      )}
      {isOpen && own && <p className="text-sm text-slate-700">You filed this request. A different person must approve it; you can cancel it.</p>}
      <p className="text-xs text-slate-500">These buttons are hints. The server decides who may approve, and every refusal is shown.</p>
      <Link to="/withdrawal-restrictions/changes" className="text-sm text-brand-700 hover:underline">&larr; Back to change requests</Link>
      {dialog && <DecisionDialog op={dialog} change={c} onClose={() => setDialog(null)} onDone={(b) => done(dialog, b)} />}
    </div>
  )
}
