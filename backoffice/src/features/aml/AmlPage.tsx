import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import {
  UUID_RE,
  listCases,
  listPersonHolds,
  liftHold,
  openCase,
  placeHold,
  type AmlHold,
  type AmlQueueItem,
  type HoldReasonClass,
  type HoldScope,
} from '../../api/aml'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { TextInput, SelectField } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Table, type Column } from '../../components/Table'
import { AmlErrorAlert, ConfidentialityBanner, ReasonDialog, shortId, useAmlTenant } from './AmlBits'
import { describeAmlError } from './errors'
import { isClosed, statusLabel } from './rules'

type Tab = 'cases' | 'holds'

const SCOPE_LABEL: Record<string, string> = { all_value_movement: 'All value movement', withdrawal_and_payout: 'Withdrawal and payout' }
const CLASS_LABEL: Record<string, string> = { manual: 'Manual', confirmed_sanctions: 'Sanctions list (four-eyes lift)' }

/**
 * AML officer console (ADR 0116 S6, minimal): the case queue and a person's holds. Route-gated by the `amlRead` nav flag as a
 * convenience only; the server is authoritative (aml_officer grant + signed proof + quorum). Every refusal goes through the
 * closed error map and the server message is never displayed. Step-up prompts come from apiFetch (StepUpProvider).
 */
export function AmlPage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const { tenantId, picker } = useAmlTenant()
  const [tab, setTab] = useState<Tab>('cases')

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="AML" description="Cases and holds. Officer access only; every action is checked and logged by the server." />
      <ConfidentialityBanner />
      {picker}
      {tenantId === '' && <p className="text-sm text-slate-500">Choose a tenant to work its AML queue.</p>}
      {tenantId !== '' && (
        <>
          <div role="tablist" aria-label="AML sections" className="flex gap-2 border-b border-border">
            {(['cases', 'holds'] as Tab[]).map((t) => (
              <button
                key={t}
                role="tab"
                aria-selected={tab === t}
                onClick={() => setTab(t)}
                className={`px-3 py-2 text-sm font-medium ${tab === t ? 'border-b-2 border-brand-600 text-brand-700' : 'text-slate-500 hover:text-slate-700'}`}
              >
                {t === 'cases' ? 'Cases' : 'Holds'}
              </button>
            ))}
          </div>
          {tab === 'cases' ? <CasesTab tenantId={tenantId} canWork={perms.amlWork} /> : <HoldsTab tenantId={tenantId} canManage={perms.amlHolds} />}
        </>
      )}
    </div>
  )
}

function statusTone(s: string) {
  return isClosed(s as never) ? 'neutral' : s === 'sar_decision_pending' || s === 'escalated' ? 'warning' : 'info'
}

function CasesTab({ tenantId, canWork }: { tenantId: string; canWork: boolean }) {
  const qc = useQueryClient()
  const queue = useQuery({ queryKey: ['aml', 'queue', tenantId], queryFn: () => listCases(tenantId, 50), retry: false })
  const [opening, setOpening] = useState(false)
  const [person, setPerson] = useState('')
  const [notice, setNotice] = useState('')
  const open = useMutation({
    mutationFn: (reason: string) => openCase(tenantId, { person_id: person.trim(), kind: 'manual', reason_code: reason }),
    onSuccess: () => {
      setOpening(false)
      setPerson('')
      setNotice('Case opened.')
      void qc.invalidateQueries({ queryKey: ['aml', 'queue'] })
    },
  })
  const columns: Column<AmlQueueItem>[] = [
    { key: 'case', header: 'Case', render: (r) => <Link className="font-mono text-xs text-brand-700 hover:underline" to={`/aml/cases/${r.case_id}?tenant=${tenantId}`}>{shortId(r.case_id)}</Link> },
    { key: 'person', header: 'Person', render: (r) => <span className="font-mono text-xs">{shortId(r.person_id)}</span> },
    { key: 'status', header: 'Status', render: (r) => <Badge tone={statusTone(r.status)}>{statusLabel(r.status)}</Badge> },
    { key: 'sar', header: 'SAR', render: (r) => (r.sar_flagged ? <Badge tone="danger">Flagged</Badge> : <span className="text-slate-400">-</span>) },
    { key: 'scope', header: 'Owner', render: (r) => r.owner_scope },
    { key: 'opened', header: 'Opened', render: (r) => new Date(r.opened_at).toLocaleString() },
  ]
  return (
    <section aria-label="Cases" className="flex flex-col gap-3">
      <div className="flex gap-2">
        <Button variant="secondary" onClick={() => { setNotice(''); void queue.refetch() }} isLoading={queue.isFetching}>Refresh</Button>
        {canWork && <Button onClick={() => { setNotice(''); open.reset(); setOpening(true) }}>Open a case</Button>}
      </div>
      {notice && <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">{notice}</p>}
      {queue.isLoading && <LoadingSpinner label="Loading cases..." />}
      {queue.error != null && <AmlErrorAlert error={queue.error} op="queue" />}
      {queue.data && <Table columns={columns} rows={queue.data.items} getRowKey={(r) => r.case_id} emptyMessage="No cases in your scope." />}
      {opening && (
        <ReasonDialog
          title="Open an AML case"
          description="Opens a manual case on a person. The person is identified by id; you cannot open a case about yourself."
          confirmLabel="Open case"
          isSubmitting={open.isPending}
          error={open.error}
          op="open_case"
          extra={<TextInput label="Person id" value={person} onChange={(e) => setPerson(e.target.value)} autoComplete="off" />}
          extraValid={UUID_RE.test(person.trim())}
          onConfirm={(r) => open.mutate(r)}
          onCancel={() => setOpening(false)}
        />
      )}
    </section>
  )
}

function HoldsTab({ tenantId, canManage }: { tenantId: string; canManage: boolean }) {
  const qc = useQueryClient()
  const [personInput, setPersonInput] = useState('')
  const [person, setPerson] = useState('')
  const [placing, setPlacing] = useState(false)
  const [lifting, setLifting] = useState<AmlHold | null>(null)
  const [scope, setScope] = useState<HoldScope>('withdrawal_and_payout')
  const [cls, setCls] = useState<HoldReasonClass>('manual')
  const [notice, setNotice] = useState('')
  const holds = useQuery({
    queryKey: ['aml', 'holds', tenantId, person],
    queryFn: () => listPersonHolds(tenantId, person),
    enabled: person !== '',
    retry: false,
  })
  const place = useMutation({
    mutationFn: (reason: string) => placeHold(tenantId, { person_id: person, scope, reason_class: cls, reason_code: reason }),
    onSuccess: () => {
      setPlacing(false)
      setNotice('Hold placed.')
      void qc.invalidateQueries({ queryKey: ['aml', 'holds'] })
    },
    onError: (e) => {
      if (describeAmlError(e, 'place_hold').stale) void qc.invalidateQueries({ queryKey: ['aml', 'holds'] })
    },
  })
  const lift = useMutation({
    mutationFn: (reason: string) => liftHold(tenantId, lifting!.id, reason),
    onSuccess: () => {
      setLifting(null)
      setNotice('Hold lifted.')
      void qc.invalidateQueries({ queryKey: ['aml', 'holds'] })
    },
    onError: (e) => {
      if (describeAmlError(e, 'lift_hold').stale) void qc.invalidateQueries({ queryKey: ['aml', 'holds'] })
    },
  })
  const personOk = UUID_RE.test(personInput.trim())
  const columns: Column<AmlHold>[] = [
    { key: 'id', header: 'Hold', render: (h) => <span className="font-mono text-xs">{shortId(h.id)}</span> },
    { key: 'scope', header: 'Gates', render: (h) => SCOPE_LABEL[h.scope] ?? 'Unrecognised' },
    { key: 'class', header: 'Class', render: (h) => CLASS_LABEL[h.reason_class] ?? 'Unrecognised' },
    { key: 'reason', header: 'Reason code', render: (h) => <span className="font-mono text-xs">{h.reason_code}</span> },
    { key: 'placed', header: 'Placed', render: (h) => new Date(h.placed_at).toLocaleString() },
    { key: 'state', header: 'State', render: (h) => (h.lifted ? <Badge>Lifted</Badge> : <Badge tone="warning">Active</Badge>) },
    {
      key: 'act',
      header: '',
      render: (h) => {
        if (h.lifted || !canManage) return null
        if (h.reason_class === 'confirmed_sanctions') return <span className="text-xs text-slate-500">Lift via a case proposal and approval</span>
        return <Button variant="secondary" onClick={() => { setNotice(''); lift.reset(); setLifting(h) }}>Lift</Button>
      },
    },
  ]
  return (
    <section aria-label="Holds" className="flex flex-col gap-3">
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          if (personOk) { setNotice(''); setPerson(personInput.trim()) }
        }}
      >
        <TextInput label="Person id" value={personInput} onChange={(e) => setPersonInput(e.target.value)} autoComplete="off" help="Holds are listed per person. There is no tenant-wide hold list." />
        <Button type="submit" disabled={!personOk}>Show holds</Button>
        {canManage && person !== '' && <Button variant="secondary" onClick={() => { setNotice(''); place.reset(); setPlacing(true) }}>Place a hold</Button>}
      </form>
      {notice && <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">{notice}</p>}
      {holds.isLoading && <LoadingSpinner label="Loading holds..." />}
      {holds.error != null && <AmlErrorAlert error={holds.error} op="holds" />}
      {holds.data && <Table columns={columns} rows={holds.data.items} getRowKey={(h) => h.id} emptyMessage="No holds for this person in your scope." />}
      {placing && (
        <ReasonDialog
          title="Place an AML hold"
          description="Restricts the person. Placing a hold needs the officer quorum for the tenant. The player and non-AML staff see only a generic review state."
          confirmLabel="Place hold"
          isSubmitting={place.isPending}
          error={place.error}
          op="place_hold"
          extra={
            <>
              <SelectField
                label="What it gates"
                value={scope}
                onChange={(e) => {
                  const v = e.target.value as HoldScope
                  setScope(v)
                  if (v !== 'all_value_movement') setCls('manual')
                }}
                options={[
                  { value: 'withdrawal_and_payout', label: SCOPE_LABEL.withdrawal_and_payout },
                  { value: 'all_value_movement', label: SCOPE_LABEL.all_value_movement },
                ]}
              />
              <SelectField
                label="Class"
                value={cls}
                disabled={scope !== 'all_value_movement'}
                onChange={(e) => setCls(e.target.value as HoldReasonClass)}
                options={[
                  { value: 'manual', label: CLASS_LABEL.manual },
                  { value: 'confirmed_sanctions', label: CLASS_LABEL.confirmed_sanctions },
                ]}
                help="A sanctions-list hold gates all value movement and can be lifted only by a four-eyes proposal and approval."
              />
            </>
          }
          onConfirm={(r) => place.mutate(r)}
          onCancel={() => setPlacing(false)}
        />
      )}
      {lifting && (
        <ReasonDialog
          title="Lift an AML hold"
          description="Releases the restriction on this person. This needs a fresh second-factor check."
          confirmLabel="Lift hold"
          isSubmitting={lift.isPending}
          error={lift.error}
          op="lift_hold"
          onConfirm={(r) => lift.mutate(r)}
          onCancel={() => setLifting(null)}
        />
      )}
    </section>
  )
}
