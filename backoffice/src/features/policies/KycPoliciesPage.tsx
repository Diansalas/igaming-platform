import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import {
  ASSET_CODE_PATTERN,
  THRESHOLD_PATTERN,
  activateKycEnforcementPolicy,
  classifyKycPolicyWrite,
  createKycEnforcementPolicy,
  listKycEnforcementPolicies,
  supersedeKycEnforcementPolicy,
  withdrawKycEnforcementPolicy,
  type AuthorableTriggerType,
  type CreateKycPolicyBody,
  type KycEnforcementPolicy,
  type KycPolicyStatus,
  type PlayOperation,
} from '../../api/kycEnforcementPolicies'
import { isReasonCode, listJurisdictions, type Jurisdiction } from '../../api/licensing'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { Badge } from '../../components/Badge'
import { Pending, Success, short } from '../licensing/LicensingBits'
import { KYC_DORMANT_NOTE, KYC_FOUR_EYES_NOTE, KYC_NOT_WIRED_NOTE, NO_VALUE_FROM_PLATFORM, kycStatus, playOpLabel, safeToken, triggerLabel } from './labels'
import { Note, OUTCOME_UNCONFIRMED, PolicyErrorAlert, ReasonCodeField } from './PolicyBits'
import type { PolicyCtx } from './errors'

const LEGAL_REF_MAX = 200
// No surrounding space and no control characters (OpenAPI description of legal_review_reference).
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/
export const isLegalReference = (s: string) => s.length >= 1 && s.length <= LEGAL_REF_MAX && s === s.trim() && !CONTROL.test(s)

const KEY = (p: KycEnforcementPolicy) => [p.licensing_jurisdiction_id, p.trigger_type, p.play_operation ?? '', p.asset_code ?? ''].join('|')

function PolicyValue({ p }: { p: KycEnforcementPolicy }) {
  if (p.trigger_type === 'cumulative_deposit') {
    return (
      <span>
        Threshold <span className="font-mono">{safeToken(p.threshold_minor_units ?? '')}</span> minor units of <span className="font-mono">{safeToken(p.asset_code ?? '')}</span>
      </span>
    )
  }
  if (p.trigger_type === 'play') return <span>{playOpLabel(p.play_operation)}</span>
  return <span>Not applicable</span>
}

type Action = { kind: 'activate' | 'withdraw' | 'supersede'; policy: KycEnforcementPolicy }

function CreateDialog({ jurisdictions, onClose, onDone }: { jurisdictions: Jurisdiction[]; onClose: () => void; onDone: (p: KycEnforcementPolicy | null) => void }) {
  const [jur, setJur] = useState('')
  const [trigger, setTrigger] = useState<AuthorableTriggerType | ''>('')
  const [threshold, setThreshold] = useState('')
  const [asset, setAsset] = useState('')
  const [op, setOp] = useState<PlayOperation | ''>('')
  const [legal, setLegal] = useState('')
  const [reason, setReason] = useState('')
  const sending = useRef(false)
  const m = useMutation({
    mutationFn: () => {
      const body: CreateKycPolicyBody = { licensing_jurisdiction_id: jur, trigger_type: trigger as AuthorableTriggerType, legal_review_reference: legal, reason_code: reason }
      if (trigger === 'cumulative_deposit') {
        body.threshold_minor_units = threshold
        body.asset_code = asset
      } else {
        body.play_operation = op as PlayOperation
      }
      return createKycEnforcementPolicy(body)
    },
    onSuccess: (r) => {
      const c = classifyKycPolicyWrite(r, 'draft')
      onDone(c.kind === 'confirmed' ? c.policy : null)
    },
    onSettled: () => {
      sending.current = false
    },
  })
  const detailOk = trigger === 'cumulative_deposit' ? THRESHOLD_PATTERN.test(threshold) && ASSET_CODE_PATTERN.test(asset) : trigger === 'play' ? op !== '' : false
  const valid = jur !== '' && trigger !== '' && detailOk && isLegalReference(legal) && isReasonCode(reason)
  return (
    <Modal title="Author a KYC enforcement policy (draft)" onClose={onClose} size="lg">
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
        <Note label="Draft notice" tone="slate">A draft governs nothing. It needs a different platform administrator to activate it, and a fresh second-factor check is needed to author it.</Note>
        <SelectField
          label="Licence jurisdiction"
          value={jur}
          onChange={(e) => setJur(e.target.value)}
          options={[{ value: '', label: 'Choose a jurisdiction' }, ...jurisdictions.map((j) => ({ value: j.id, label: `${j.code} - ${j.name}` }))]}
        />
        <SelectField
          label="Trigger type"
          value={trigger}
          onChange={(e) => {
            setTrigger(e.target.value as AuthorableTriggerType | '')
            setThreshold('')
            setAsset('')
            setOp('')
          }}
          options={[
            { value: '', label: 'Choose a trigger type' },
            { value: 'cumulative_deposit', label: 'Cumulative deposit' },
            { value: 'play', label: 'Play' },
          ]}
          help={KYC_NOT_WIRED_NOTE}
        />
        {trigger === 'cumulative_deposit' && (
          <>
            <TextInput
              label="Threshold in minor units"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value.trim())}
              inputMode="numeric"
              autoComplete="off"
              maxLength={38}
              help="A positive whole number of the asset's smallest units (for example cents), digits only, no decimal point and no separators. Entered by you from the compliance decision; there is no default."
            />
            {threshold !== '' && !THRESHOLD_PATTERN.test(threshold) && <p role="alert" className="text-sm text-red-700">Use digits only, starting with 1 to 9, at most 38 digits.</p>}
            <TextInput label="Asset code" value={asset} onChange={(e) => setAsset(e.target.value.trim())} autoComplete="off" maxLength={32} help="Must be a registered asset; the server refuses an unknown one." />
            {asset !== '' && !ASSET_CODE_PATTERN.test(asset) && <p role="alert" className="text-sm text-red-700">Use letters, digits, _ . - only.</p>}
          </>
        )}
        {trigger === 'play' && (
          <SelectField
            label="Play operation"
            value={op}
            onChange={(e) => setOp(e.target.value as PlayOperation | '')}
            options={[
              { value: '', label: 'Choose an operation' },
              { value: 'casino_play', label: 'Casino play' },
              { value: 'sportsbook_play', label: 'Sportsbook play' },
            ]}
          />
        )}
        <TextInput
          label="Legal review reference"
          value={legal}
          onChange={(e) => setLegal(e.target.value)}
          autoComplete="off"
          maxLength={LEGAL_REF_MAX}
          help="An opaque reference to the human legal review that supplied the value (1 to 200 characters, no surrounding space). The platform does not verify it."
        />
        {legal !== '' && !isLegalReference(legal) && <p role="alert" className="text-sm text-red-700">1 to 200 characters, no space at either end, no control characters.</p>}
        <ReasonCodeField value={reason} onChange={setReason} />
        {m.error && <PolicyErrorAlert error={m.error} ctx="kyc_author" />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending}>Create draft</Button>
        </div>
      </form>
    </Modal>
  )
}

function ActionDialog({ action, onClose, onDone }: { action: Action; onClose: () => void; onDone: (a: Action, ok: boolean) => void }) {
  const { claims } = useAuth()
  const p = action.policy
  const [reason, setReason] = useState('')
  const [successor, setSuccessor] = useState('')
  const sending = useRef(false)
  const drafts = useQuery({
    queryKey: ['kyc-policies', 'successors', p.licensing_jurisdiction_id],
    queryFn: () => listKycEnforcementPolicies({ licensing_jurisdiction_id: p.licensing_jurisdiction_id, status: 'draft' }),
    enabled: action.kind === 'supersede',
    retry: false,
  })
  const candidates = (drafts.data ?? []).filter((d) => d.id !== p.id && KEY(d) === KEY(p))
  const m = useMutation({
    mutationFn: async () => {
      if (action.kind === 'activate') return { r: await activateKycEnforcementPolicy(p.id, reason), expect: 'active' as KycPolicyStatus, id: p.id }
      if (action.kind === 'withdraw') return { r: await withdrawKycEnforcementPolicy(p.id, reason), expect: 'withdrawn' as KycPolicyStatus, id: p.id }
      return { r: await supersedeKycEnforcementPolicy(p.id, successor, reason), expect: 'active' as KycPolicyStatus, id: successor }
    },
    onSuccess: ({ r, expect, id }) => onDone(action, classifyKycPolicyWrite(r, expect, id).kind === 'confirmed'),
    onSettled: () => {
      sending.current = false
    },
  })
  const own = !!claims?.sub && p.created_by === claims.sub
  const valid = isReasonCode(reason) && (action.kind !== 'supersede' || successor !== '')
  const ctx: PolicyCtx = action.kind === 'activate' ? 'kyc_activate' : action.kind === 'withdraw' ? 'kyc_withdraw' : 'kyc_supersede'
  const title = action.kind === 'activate' ? 'Activate KYC enforcement policy' : action.kind === 'withdraw' ? 'Withdraw draft KYC enforcement policy' : 'Supersede active KYC enforcement policy'
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
        <p className="text-sm">
          {triggerLabel(p.trigger_type)} - policy <span className="font-mono text-xs">{short(p.id)}</span>: <PolicyValue p={p} />
        </p>
        {action.kind === 'activate' && (
          <>
            <Note label="Activation notice" tone="slate">Activating makes this policy enforce KYC for its jurisdiction. The database refuses the activation if you authored this draft: a different platform administrator must do it.</Note>
            {own && <p role="note" className="text-sm text-amber-900">You authored this draft. The server will refuse your activation.</p>}
          </>
        )}
        {action.kind === 'withdraw' && <Note label="Withdraw notice" tone="slate">Withdrawing a draft is final. It is only possible for a policy that was never activated.</Note>}
        {action.kind === 'supersede' && (
          <>
            <Note label="Supersede notice" tone="slate">This is the only way an active policy stops being enforced: one transaction withdraws it and activates the successor. The acting administrator must differ from the authors of BOTH policies.</Note>
            {own && <p role="note" className="text-sm text-amber-900">You authored the active policy. The server will refuse your supersession.</p>}
            <SelectField
              label="Successor draft"
              value={successor}
              onChange={(e) => setSuccessor(e.target.value)}
              options={[
                { value: '', label: 'Choose a successor draft' },
                ...candidates.map((d) => ({ value: d.id, label: `${short(d.id)} - ${d.trigger_type === 'cumulative_deposit' ? `${safeToken(d.threshold_minor_units ?? '')} ${safeToken(d.asset_code ?? '')}` : playOpLabel(d.play_operation)}` })),
              ]}
              help="Only drafts of the same jurisdiction, trigger type, play operation and asset are listed."
            />
            {drafts.isSuccess && candidates.length === 0 && <p className="text-sm text-slate-600">There is no draft of the same enforcement key. Author one first.</p>}
            {drafts.error ? <PolicyErrorAlert error={drafts.error} ctx="read" /> : null}
          </>
        )}
        <ReasonCodeField value={reason} onChange={setReason} />
        {m.error && <PolicyErrorAlert error={m.error} ctx={ctx} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>Close</Button>
          <Button type="submit" disabled={!valid} isLoading={m.isPending} variant={action.kind === 'withdraw' ? 'danger' : 'primary'}>
            {action.kind === 'activate' ? 'Activate' : action.kind === 'withdraw' ? 'Withdraw draft' : 'Supersede'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

export function KycPoliciesPage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const qc = useQueryClient()
  const [jur, setJur] = useState('')
  const [status, setStatus] = useState<KycPolicyStatus | ''>('')
  const [creating, setCreating] = useState(false)
  const [action, setAction] = useState<Action | null>(null)
  const [outcome, setOutcome] = useState<{ kind: 'created'; policy: KycEnforcementPolicy | null } | { kind: 'acted'; action: Action['kind']; ok: boolean } | null>(null)

  const jurisdictions = useQuery({ queryKey: ['kyc-policies', 'jurisdictions'], queryFn: listJurisdictions, retry: false })
  const list = useQuery({
    queryKey: ['kyc-policies', 'list', jur, status],
    queryFn: () => listKycEnforcementPolicies({ licensing_jurisdiction_id: jur || undefined, status: status || undefined }),
    retry: false,
  })
  const jurName = (id: string) => {
    const j = (jurisdictions.data ?? []).find((x) => x.id === id)
    return j ? `${safeToken(j.code)} - ${j.name}` : short(id)
  }
  const rows = list.data ?? []

  const columns: Column<KycEnforcementPolicy>[] = [
    { key: 'jur', header: 'Jurisdiction', render: (p) => jurName(p.licensing_jurisdiction_id) },
    { key: 'trig', header: 'Trigger', render: (p) => triggerLabel(p.trigger_type) },
    { key: 'val', header: 'Value (from compliance)', render: (p) => <PolicyValue p={p} /> },
    { key: 'status', header: 'Status', render: (p) => { const s = kycStatus(p.status); return <Badge tone={s.tone}>{s.label}</Badge> } },
    { key: 'legal', header: 'Legal review reference', render: (p) => (p.legal_review_reference ? <span className="text-xs">{p.legal_review_reference}</span> : 'None') },
    { key: 'reason', header: 'Reason code', render: (p) => <span className="font-mono text-xs">{safeToken(p.reason_code)}</span> },
    { key: 'by', header: 'Author', render: (p) => (p.created_by ? `${short(p.created_by)}${claims?.sub === p.created_by ? ' (you)' : ''}` : 'Not shown') },
    { key: 'succ', header: 'Superseded by', render: (p) => (p.successor_policy_id ? short(p.successor_policy_id) : '') },
    {
      key: 'act',
      header: 'Actions',
      render: (p) =>
        perms.kycPolicyManage ? (
          <div className="flex gap-2">
            {p.status === 'draft' && <Button variant="secondary" onClick={() => { setOutcome(null); setAction({ kind: 'activate', policy: p }) }}>Activate</Button>}
            {p.status === 'draft' && <Button variant="secondary" onClick={() => { setOutcome(null); setAction({ kind: 'withdraw', policy: p }) }}>Withdraw draft</Button>}
            {p.status === 'active' && <Button variant="secondary" onClick={() => { setOutcome(null); setAction({ kind: 'supersede', policy: p }) }}>Supersede</Button>}
          </div>
        ) : null,
    },
  ]

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="KYC enforcement policies"
        description="Platform-wide thresholds that make KYC mandatory, per licence jurisdiction."
        actions={perms.kycPolicyManage ? <Button onClick={() => { setOutcome(null); setCreating(true) }}>Author a policy</Button> : undefined}
      />
      <Note label="Source of values">{NO_VALUE_FROM_PLATFORM}</Note>
      <Note label="Dormant by default" tone="slate">{KYC_DORMANT_NOTE}</Note>
      <Note label="Four-eyes" tone="slate">{KYC_FOUR_EYES_NOTE}</Note>
      <Card title="Filter">
        <div className="flex flex-wrap items-end gap-3">
          <SelectField label="Licence jurisdiction" value={jur} onChange={(e) => setJur(e.target.value)} options={[{ value: '', label: 'All jurisdictions' }, ...(jurisdictions.data ?? []).map((j) => ({ value: j.id, label: `${j.code} - ${j.name}` }))]} />
          <SelectField
            label="Status"
            value={status}
            onChange={(e) => setStatus(e.target.value as KycPolicyStatus | '')}
            options={[{ value: '', label: 'Any status' }, { value: 'draft', label: 'Draft' }, { value: 'active', label: 'Active' }, { value: 'withdrawn', label: 'Withdrawn' }]}
          />
        </div>
        {jurisdictions.error ? <PolicyErrorAlert error={jurisdictions.error} ctx="read" /> : null}
      </Card>
      {outcome?.kind === 'created' &&
        (outcome.policy ? (
          <Success>Draft created ({short(outcome.policy.id)}). It governs nothing until a different platform administrator activates it.</Success>
        ) : (
          <Pending>{OUTCOME_UNCONFIRMED}</Pending>
        ))}
      {outcome?.kind === 'acted' &&
        (outcome.ok ? (
          <Success>
            {outcome.action === 'activate' ? 'Activated. This policy now enforces KYC for its jurisdiction.' : outcome.action === 'withdraw' ? 'Draft withdrawn.' : 'Superseded. The successor is now the active policy and the previous one is withdrawn.'}
          </Success>
        ) : (
          <Pending>{OUTCOME_UNCONFIRMED}</Pending>
        ))}
      {list.error ? <PolicyErrorAlert error={list.error} ctx="read" /> : null}
      <Table
        columns={columns}
        rows={rows}
        getRowKey={(p) => p.id}
        isLoading={list.isLoading}
        emptyMessage="No KYC enforcement policy is recorded for this filter. With no active policy, KYC enforcement is dormant for the jurisdiction."
      />
      {rows.length >= 500 && <p className="text-xs text-slate-600">The newest 500 policies are shown. Narrow the filter to see others.</p>}
      <p className="text-xs text-slate-500">The buttons are hints. The server decides who may act, and every refusal is shown.</p>
      {creating && (
        <CreateDialog
          jurisdictions={jurisdictions.data ?? []}
          onClose={() => setCreating(false)}
          onDone={(policy) => {
            setCreating(false)
            setOutcome({ kind: 'created', policy })
            void qc.invalidateQueries({ queryKey: ['kyc-policies'] })
          }}
        />
      )}
      {action && (
        <ActionDialog
          action={action}
          onClose={() => setAction(null)}
          onDone={(a, ok) => {
            setAction(null)
            setOutcome({ kind: 'acted', action: a.kind, ok })
            void qc.invalidateQueries({ queryKey: ['kyc-policies'] })
          }}
        />
      )}
    </div>
  )
}
