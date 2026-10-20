import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { isReasonCode } from '../../api/licensing'
import {
  RESIDENCE_MODES,
  approveResidenceDetermination,
  approveResidencePolicy,
  classifyResidencePolicyApproval,
  classifyResidenceSet,
  getResidencePolicy,
  isDeterminationApproved,
  listPendingResidenceApprovals,
  modeRank,
  setResidencePolicy,
  type PendingResidenceApproval,
  type ResidenceMode,
  type ResidencePolicyVersion,
} from '../../api/residenceApproval'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { SelectField } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { Pending, Success, short, } from '../licensing/LicensingBits'
import { when } from '../licensing/labels'
import { RESIDENCE_MODE_TEXT, RESIDENCE_PENDING_NOTE, RESIDENCE_TIGHTEN_NOTE, modeShort, safeToken } from './labels'
import { Note, OUTCOME_UNCONFIRMED, PolicyErrorAlert, ReasonCodeField } from './PolicyBits'

type ModeOrEmpty = ResidenceMode | ''

function SetModeCard({ tenantId, effective }: { tenantId: string; effective: ResidenceMode }) {
  const qc = useQueryClient()
  const [mode, setMode] = useState<ModeOrEmpty>('')
  const [reason, setReason] = useState('')
  const [result, setResult] = useState<{ text: string; pending: boolean; ok: boolean } | null>(null)
  const sending = useRef(false)
  const m = useMutation({
    mutationFn: () => setResidencePolicy(tenantId, mode as ResidenceMode, reason),
    onSuccess: (body) => {
      const c = classifyResidenceSet(body, mode as ResidenceMode)
      if (c.kind === 'applied') {
        setResult({ ok: true, pending: false, text: `Applied at once: the mode in force is now "${modeShort(c.change.requested_mode)}" (it was "${modeShort(c.change.previous_mode)}"). It applies to future determinations only.` })
      } else if (c.kind === 'pending') {
        setResult({
          ok: true,
          pending: true,
          text: `Pending approval. Nothing has changed yet: the mode in force is still "${modeShort(c.change.previous_mode)}". "${modeShort(c.change.requested_mode)}" is less strict and needs ${c.change.required_approvals} approvals from 2 different people, neither the requester. Approve it below.`,
        })
      } else {
        setResult({ ok: false, pending: true, text: OUTCOME_UNCONFIRMED })
      }
      setMode('')
      setReason('')
      void qc.invalidateQueries({ queryKey: ['residence'] })
    },
    onSettled: () => {
      sending.current = false
    },
  })
  const same = mode !== '' && mode === effective
  const valid = mode !== '' && !same && isReasonCode(reason)
  const hint = mode === '' || same ? null : modeRank(mode) > modeRank(effective) ? 'stricter' : 'looser'
  return (
    <Card title="Change the mode">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          setResult(null)
          m.mutate()
        }}
      >
        <p className="text-sm text-slate-700">
          Choose the mode explicitly. Nothing is preselected and the platform does not recommend one: whether a second Person must approve residence determinations is your compliance function's decision.
        </p>
        <SelectField
          label="New mode"
          value={mode}
          onChange={(e) => setMode(e.target.value as ModeOrEmpty)}
          options={[{ value: '', label: 'Choose a mode' }, ...RESIDENCE_MODES.map((x) => ({ value: x, label: modeShort(x) }))]}
          help={mode !== '' ? RESIDENCE_MODE_TEXT[mode] : 'Order from least to most strict: None, Into open market, All.'}
        />
        {same && <p role="alert" className="text-sm text-red-700">That is already the mode in force.</p>}
        {hint === 'stricter' && <Note label="Class hint" tone="slate">Expected: a stricter mode. It would be active at once, with one actor. The database makes the final classification.</Note>}
        {hint === 'looser' && <Note label="Class hint" tone="slate">Expected: a less strict mode. It would be filed PENDING and take effect only after 2 approvals by 2 different people, neither the requester. The mode in force does not change until then. The database makes the final classification.</Note>}
        <ReasonCodeField value={reason} onChange={setReason} />
        <Note label="Applies to future determinations" tone="slate">{RESIDENCE_TIGHTEN_NOTE}</Note>
        {m.error && <PolicyErrorAlert error={m.error} ctx="residence_set" />}
        {result && (result.ok && !result.pending ? <Success>{result.text}</Success> : <Pending>{result.text}</Pending>)}
        <div className="flex justify-end">
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>File the change</Button>
        </div>
      </form>
    </Card>
  )
}

function PolicyApproveDialog({ tenantId, version, onClose, onDone }: { tenantId: string; version: ResidencePolicyVersion; onClose: () => void; onDone: (text: string, ok: boolean) => void }) {
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const m = useMutation({
    mutationFn: () => approveResidencePolicy(tenantId, version.id, reason),
    onSuccess: (body) => {
      const c = classifyResidencePolicyApproval(body)
      if (c.kind === 'activated') onDone(`Approval recorded (${c.approval.approvals} of ${c.approval.required_approvals}) and the mode "${modeShort(version.mode)}" is now in force.`, true)
      else if (c.kind === 'recorded') onDone(`Approval recorded: ${c.approval.approvals} of ${c.approval.required_approvals}. Still pending - the mode in force has not changed.`, false)
      else onDone(OUTCOME_UNCONFIRMED, false)
    },
    onSettled: () => {
      sending.current = false
    },
  })
  const valid = isReasonCode(reason)
  return (
    <Modal title="Approve residence mode change" onClose={onClose}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <p className="text-sm">
          Change from "{modeShort(version.previous_mode)}" to "{modeShort(version.mode)}" - request <span className="font-mono text-xs">{short(version.id)}</span>, {version.approvals} of {version.required_approvals} approvals so far.
        </p>
        <Note label="Approval rules" tone="slate">The requester, and any other account of the requester's Person, cannot approve. One Person approves once. Your account must be linked to a Person. The request is only valid while the mode in force is still "{modeShort(version.previous_mode)}". Approving needs a fresh second-factor check.</Note>
        <ReasonCodeField value={reason} onChange={setReason} />
        {m.error && <PolicyErrorAlert error={m.error} ctx="residence_policy_approve" />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>Approve</Button>
        </div>
      </form>
    </Modal>
  )
}

function DeterminationApproveDialog({ row, onClose, onDone }: { row: PendingResidenceApproval; onClose: () => void; onDone: (ok: boolean) => void }) {
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const m = useMutation({
    mutationFn: () => approveResidenceDetermination(row.verification_id, row.determined_at, reason),
    onSuccess: (body) => onDone(isDeterminationApproved(body, row.verification_id)),
    onSettled: () => {
      sending.current = false
    },
  })
  const valid = isReasonCode(reason)
  return (
    <Modal title="Approve residence determination" onClose={onClose}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (!valid || sending.current) return
          sending.current = true
          m.mutate()
        }}
      >
        <p className="text-sm">
          Verification <span className="font-mono text-xs">{short(row.verification_id)}</span>, determined {when(row.determined_at)}. Approving makes this determination market evidence for the player.
        </p>
        <Note label="Second Person" tone="slate">You are the second Person. You must be a different staff account AND a different, linked Person than the one who made the determination. The approval is bound to the exact determination time listed here; a newer determination replaces it. Approving needs a fresh second-factor check.</Note>
        <ReasonCodeField value={reason} onChange={setReason} />
        {m.error && <PolicyErrorAlert error={m.error} ctx="residence_determination_approve" />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>Approve determination</Button>
        </div>
      </form>
    </Modal>
  )
}

export function ResidencePage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const tenantId = claims?.tenant_id ?? ''
  const [approvePolicy, setApprovePolicy] = useState<ResidencePolicyVersion | null>(null)
  const [approveRow, setApproveRow] = useState<PendingResidenceApproval | null>(null)
  const [notice, setNotice] = useState<{ text: string; ok: boolean } | null>(null)

  const policy = useQuery({ queryKey: ['residence', 'policy', tenantId], queryFn: () => getResidencePolicy(tenantId), enabled: !!tenantId && perms.residencePolicyRead, retry: false })
  const pending = useQuery({ queryKey: ['residence', 'pending'], queryFn: listPendingResidenceApprovals, enabled: perms.residenceQueue, retry: false })

  const versionCols: Column<ResidencePolicyVersion>[] = [
    { key: 'mode', header: 'Mode', render: (v) => modeShort(v.mode) },
    { key: 'prev', header: 'Changed from', render: (v) => modeShort(v.previous_mode) },
    { key: 'status', header: 'Status', render: (v) => (v.status === 'active' ? <Badge tone="success">Active</Badge> : <Badge tone="warning">Pending approval</Badge>) },
    { key: 'appr', header: 'Approvals', render: (v) => `${v.approvals} of ${v.required_approvals}` },
    { key: 'reason', header: 'Reason code', render: (v) => <span className="font-mono text-xs">{safeToken(v.reason_code)}</span> },
    { key: 'by', header: 'Requested by', render: (v) => (v.requested_by ? `${short(v.requested_by)}${claims?.sub === v.requested_by ? ' (you)' : ''}` : 'Not shown') },
    { key: 'at', header: 'Requested', render: (v) => when(v.requested_at) },
    { key: 'act', header: 'Activated', render: (v) => (v.activated_at ? when(v.activated_at) : 'Not yet') },
    {
      key: 'do',
      header: 'Actions',
      render: (v) =>
        perms.residencePolicyManage && v.status === 'pending' ? (
          <Button
            variant="secondary"
            disabled={!!claims?.sub && v.requested_by === claims.sub}
            title={v.requested_by === claims?.sub ? 'You filed this request; a different person must approve it.' : undefined}
            onClick={() => { setNotice(null); setApprovePolicy(v) }}
          >
            Approve
          </Button>
        ) : null,
    },
  ]

  const queueCols: Column<PendingResidenceApproval>[] = [
    { key: 'v', header: 'Verification', render: (r) => <span className="font-mono text-xs">{short(r.verification_id)}</span> },
    { key: 'p', header: 'Player', render: (r) => <span className="font-mono text-xs">{short(r.player_account_id)}</span> },
    { key: 'b', header: 'Brand', render: (r) => <span className="font-mono text-xs">{short(r.brand_id)}</span> },
    { key: 'at', header: 'Determined', render: (r) => <span title="Exact time passed back on approval">{when(r.determined_at)}</span> },
    { key: 'by', header: 'Determined by', render: (r) => (r.determined_by ? `${short(r.determined_by)}${claims?.sub === r.determined_by ? ' (you)' : ''}` : 'Not shown') },
    { key: 'mode', header: 'Mode that required approval', render: (r) => modeShort(r.policy_mode) },
    { key: 'st', header: 'Evidence status', render: () => <Badge tone="warning">Not market evidence</Badge> },
    {
      key: 'do',
      header: 'Actions',
      render: (r) =>
        perms.residencePolicyManage ? (
          <Button
            variant="secondary"
            disabled={!!claims?.sub && r.determined_by === claims.sub}
            title={r.determined_by === claims?.sub ? 'You made this determination; a different person must approve it.' : undefined}
            onClick={() => { setNotice(null); setApproveRow(r) }}
          >
            Approve as second Person
          </Button>
        ) : null,
    },
  ]

  if (policy.isLoading) return <LoadingSpinner label="Loading residence approval policy..." />
  const effective = policy.data?.effective_mode

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Residence determination approval" description="Whether a second Person must approve a reviewer's residence determination before it counts as market evidence." />
      <Note label="Source of values">Which mode applies is a decision of your compliance function. The platform ships no policy: a tenant that never sets one stays on "None", where a single reviewer's determination is market evidence at once. Nothing on this page is a legal or regulatory authorisation.</Note>
      {policy.error ? <PolicyErrorAlert error={policy.error} ctx="read" /> : null}
      {effective && (
        <Card title="Mode in force">
          <p className="text-sm font-medium">{modeShort(effective)}</p>
          <p className="mt-1 text-sm text-slate-700">{RESIDENCE_MODE_TEXT[effective]}</p>
          {effective === 'none' && <p className="mt-2 text-xs text-slate-600">No second approval is required at present. This is the default state of every tenant.</p>}
        </Card>
      )}
      {notice && (notice.ok ? <Success>{notice.text}</Success> : <Pending>{notice.text}</Pending>)}
      {effective && perms.residencePolicyManage && <SetModeCard tenantId={tenantId} effective={effective} />}
      {effective && !perms.residencePolicyManage && <p className="text-sm text-slate-600">Your role can read this policy. Only compliance staff of the tenant can change it or approve.</p>}
      {policy.data && (
        <Card title="History and pending requests">
          <Table columns={versionCols} rows={policy.data.versions} getRowKey={(v) => v.id} emptyMessage="No mode has ever been set for this tenant." />
          {policy.data.versions.length >= 200 && <p className="mt-2 text-xs text-slate-600">The newest 200 versions are shown.</p>}
        </Card>
      )}
      {perms.residenceQueue && (
        <Card title="Determinations waiting for a second Person">
          <div className="mb-3"><Note label="Pending determinations are not evidence">{RESIDENCE_PENDING_NOTE}</Note></div>
          {pending.error ? <PolicyErrorAlert error={pending.error} ctx="read" /> : null}
          <Table columns={queueCols} rows={pending.data ?? []} getRowKey={(r) => `${r.verification_id}|${r.determined_at}`} isLoading={pending.isLoading} emptyMessage="No determination is waiting for a second Person." />
          {(pending.data?.length ?? 0) >= 200 && <p className="mt-2 text-xs text-slate-600">The oldest 200 are shown.</p>}
        </Card>
      )}
      <p className="text-xs text-slate-500">The buttons are hints. The server decides who may act, and every refusal is shown.</p>
      {approvePolicy && (
        <PolicyApproveDialog
          tenantId={tenantId}
          version={approvePolicy}
          onClose={() => setApprovePolicy(null)}
          onDone={(text, ok) => {
            setApprovePolicy(null)
            setNotice({ text, ok })
            void qc.invalidateQueries({ queryKey: ['residence'] })
          }}
        />
      )}
      {approveRow && (
        <DeterminationApproveDialog
          row={approveRow}
          onClose={() => setApproveRow(null)}
          onDone={(ok) => {
            setApproveRow(null)
            setNotice(ok ? { ok: true, text: 'Approved. The determination is now market evidence.' } : { ok: false, text: OUTCOME_UNCONFIRMED })
            void qc.invalidateQueries({ queryKey: ['residence'] })
          }}
        />
      )}
    </div>
  )
}
