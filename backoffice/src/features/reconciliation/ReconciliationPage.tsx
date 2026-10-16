import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import {
  KINDS_BY_STREAM,
  RECON_STATUSES,
  getStaffReconMismatch,
  getStaffReconRun,
  listCasinoReconMismatches,
  listCasinoReconRuns,
  listStaffReconMismatches,
  listStaffReconRuns,
  type CasinoReconMismatch,
  type CasinoReconRun,
  type CasinoStream,
  type StaffReconMismatch,
  type StaffReconRun,
  type StaffStream,
} from '../../api/reconciliation'
import type { PagedResponse } from '../../api/types'
import { getNavPermissions, reconciliationStreams, type ReconciliationStreamId } from '../../auth/permissions'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { formatAge, formatMinorUnits } from '../../lib/minorUnits'
import { ConsoleErrorAlert, Hint, isUuid, shortId, useConsoleTenant } from '../consoles/ConsoleBits'
import { safeCode } from '../killswitch/labels'

const PAGE = 20

const STREAM_LABEL: Record<ReconciliationStreamId, string> = {
  payment_statement: 'Payment statement',
  ledger_vs_projection: 'Ledger vs projection',
  sportsbook_settlement: 'Sportsbook settlement',
  casino_consistency: 'Casino consistency',
  casino_statement: 'Casino statement',
}

const SUBJECT_LABEL: Record<string, string> = {
  payment_attempt: 'Payment attempt',
  deposit_intent: 'Deposit intent',
  payment_receipt: 'Payment receipt',
  bet: 'Bet',
  wallet: 'Wallet',
  ledger_account: 'Ledger account',
  ledger_transaction: 'Ledger transaction',
}

const KIND_KNOWN = new Set<string>([...Object.values(KINDS_BY_STREAM).flat(), 'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win', 'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict', 'cas_unposted_provider_event', 'cas_tombstone_late_original', 'cas_mock_statement_mismatch'])

export function describeKindLabel(v: unknown): string {
  if (typeof v === 'string' && KIND_KNOWN.has(v)) return v.replace(/_/g, ' ')
  if (v === 'other') return 'Other (not in the closed vocabulary)'
  return `Unrecognised (${safeCode(v)})`
}

function statusTone(v: unknown): BadgeTone {
  return v === 'open' ? 'danger' : v === 'investigating' ? 'warning' : v === 'resolved' || v === 'clean' ? 'success' : v === 'mismatches_found' ? 'danger' : 'neutral'
}
function statusText(v: unknown): string {
  const m: Record<string, string> = { open: 'Open', investigating: 'Investigating', resolved: 'Resolved', clean: 'Clean', mismatches_found: 'Mismatches found', other: 'Other' }
  return typeof v === 'string' && Object.prototype.hasOwnProperty.call(m, v) ? m[v] : `Unrecognised (${safeCode(v)})`
}

const isCasino = (s: ReconciliationStreamId): s is CasinoStream => s === 'casino_consistency' || s === 'casino_statement'
const when = (iso: string) => (Number.isNaN(Date.parse(iso)) ? 'Unknown time' : new Date(iso).toLocaleString())

/**
 * Reconciliation views (ADR 0114 S5): runs and mismatches per stream, READ ONLY. Detection only: nothing here
 * resolves or compensates a finding (corrections are K2/K3 flows, ADR 0114 section 2.3), so there is no mutation
 * button. Staff streams (A10c): run and finding detail come from the single-item routes and the findings table can
 * be narrowed to one run with the server-side run_id filter. Facts the screen is honest about: the casino streams
 * keep the older list-only routes (detail comes from the list row, the "In selected run" column only marks loaded
 * rows) and the staff streams carry no severity.
 */
export function ReconciliationPage() {
  const { claims, platform, tenantId, picker } = useConsoleTenant()
  const perms = getNavPermissions(claims?.role)
  const streams = reconciliationStreams(claims?.role, platform)
  const [streamPick, setStreamPick] = useState<ReconciliationStreamId | ''>('')
  const stream = (streamPick && streams.includes(streamPick) ? streamPick : streams[0]) as ReconciliationStreamId | undefined
  const [runOffset, setRunOffset] = useState(0)
  const [mmOffset, setMmOffset] = useState(0)
  const [status, setStatus] = useState('')
  const [kind, setKind] = useState('')
  const [subject, setSubject] = useState('')
  const [runSel, setRunSel] = useState<string | null>(null)
  const [mmSel, setMmSel] = useState<string | null>(null)
  // Staff streams: the run whose findings the table is narrowed to (server-side run_id filter).
  const [runFilter, setRunFilter] = useState<string | null>(null)

  const casino = stream ? isCasino(stream) : false
  const needsTenant = !casino
  const ready = !!stream && (casino || tenantId !== '')
  const subjectValid = subject === '' || isUuid(subject)

  const runs = useQuery<PagedResponse<StaffReconRun | CasinoReconRun>>({
    queryKey: ['recon', 'runs', stream, tenantId, runOffset],
    queryFn: () =>
      casino ? listCasinoReconRuns({ stream: stream as CasinoStream, limit: PAGE, offset: runOffset }) : listStaffReconRuns(tenantId, { stream: stream as StaffStream, limit: PAGE, offset: runOffset }),
    enabled: ready,
    retry: false,
  })
  const mms = useQuery<PagedResponse<StaffReconMismatch | CasinoReconMismatch>>({
    queryKey: ['recon', 'mismatches', stream, tenantId, mmOffset, status, kind, subjectValid ? subject : '', runFilter ?? ''],
    queryFn: () =>
      casino
        ? listCasinoReconMismatches({ stream: stream as CasinoStream, status: status || undefined, limit: PAGE, offset: mmOffset })
        : listStaffReconMismatches(tenantId, { stream: stream as StaffStream, status: status || undefined, kind: kind || undefined, subject_id: subject && subjectValid ? subject : undefined, run_id: runFilter ?? undefined, limit: PAGE, offset: mmOffset }),
    enabled: ready && subjectValid,
    retry: false,
  })

  const runOne = useQuery({
    queryKey: ['recon', 'run', stream, tenantId, runSel ?? ''],
    queryFn: () => getStaffReconRun(tenantId, runSel as string, stream as StaffStream),
    enabled: ready && !casino && runSel !== null && isUuid(runSel),
    retry: false,
  })

  function changeStream(s: ReconciliationStreamId) {
    setStreamPick(s)
    setRunFilter(null)
    setRunOffset(0)
    setMmOffset(0)
    setKind('')
    setSubject('')
    setStatus('')
    setRunSel(null)
    setMmSel(null)
  }

  if (streams.length === 0) {
    return (
      <div className="flex flex-col gap-4">
        <PageHeader title="Reconciliation" />
        <p className="text-sm text-slate-600">Your role has no reconciliation stream. The server decides what you may read.</p>
      </div>
    )
  }

  const runRows = (runs.data?.items ?? []) as Array<StaffReconRun | CasinoReconRun>
  const mmRows = (mms.data?.items ?? []) as Array<StaffReconMismatch | CasinoReconMismatch>
  // Casino: the list row. Staff: the single-run read (the list row only while that read is in flight or has failed).
  const selectedRun = (casino ? null : runOne.data) ?? runRows.find((r) => r.id === runSel) ?? null
  const selectedMm = mmRows.find((m) => m.id === mmSel) ?? null

  const runColumns: Column<StaffReconRun | CasinoReconRun>[] = [
    { key: 'run_at', header: 'Run at', render: (r) => when(r.run_at) },
    { key: 'period', header: 'Period', render: (r) => `${when(r.period_start)} to ${when(r.period_end)}` },
    { key: 'status', header: 'Result', render: (r) => <Badge tone={statusTone(r.status)}>{statusText(r.status)}</Badge> },
    { key: 'open', header: '', render: (r) => <Button variant="secondary" onClick={() => setRunSel(r.id)} aria-label={`Open run ${shortId(r.id)}`}>Details</Button> },
  ]
  const mmColumns: Column<StaffReconMismatch | CasinoReconMismatch>[] = [
    { key: 'age', header: 'Age', render: (m) => formatAge('detected_at' in m ? m.detected_at : m.created_at) },
    { key: 'kind', header: 'Kind', render: (m) => describeKindLabel('kind' in m ? m.kind : m.mismatch_kind) },
    { key: 'status', header: 'Status', render: (m) => <Badge tone={statusTone('status' in m ? m.status : m.investigation_status)}>{statusText('status' in m ? m.status : m.investigation_status)}</Badge> },
    { key: 'severity', header: 'Severity', render: () => (casino ? 'P1' : 'Not provided') },
    ...(casino ? [{ key: 'in_run', header: 'In selected run', render: (m: StaffReconMismatch | CasinoReconMismatch) => (runSel ? (m.run_id === runSel ? 'Yes' : 'No') : '-') }] : []),
    { key: 'open', header: '', render: (m) => <Button variant="secondary" onClick={() => setMmSel(m.id)} aria-label={`Open mismatch ${shortId(m.id)}`}>Details</Button> },
  ]

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Reconciliation" description="Runs and findings per stream. Detection only: nothing here resolves or compensates a finding." />
      {needsTenant && picker}
      {casino && <p className="text-sm text-slate-600">Casino streams are read for your own tenant.</p>}
      <div role="tablist" aria-label="Reconciliation streams" className="flex flex-wrap gap-2">
        {streams.map((s) => (
          <button key={s} role="tab" type="button" aria-selected={s === stream} onClick={() => changeStream(s)} className={`rounded-md border px-3 py-1.5 text-sm ${s === stream ? 'border-brand-600 bg-brand-50 font-semibold' : 'border-border bg-white'}`}>
            {STREAM_LABEL[s]}
          </button>
        ))}
      </div>
      {needsTenant && tenantId === '' && <p className="text-sm text-slate-500">Choose a tenant to see its reconciliation data.</p>}
      {ready && (
        <>
          <h2 className="text-sm font-semibold text-slate-900">Runs</h2>
          {runs.error && <ConsoleErrorAlert error={runs.error} family="recon" op="list" />}
          <Table columns={runColumns} rows={runRows} getRowKey={(r) => r.id} isLoading={runs.isLoading} emptyMessage="No runs recorded for this stream." pagination={runs.data ? { limit: runs.data.limit, offset: runs.data.offset, total: runs.data.total, onPageChange: setRunOffset } : undefined} />
          {selectedRun && (
            <section aria-label="Run detail" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
              <div className="flex items-start justify-between"><h3 className="text-sm font-semibold">Run detail</h3><button type="button" className="text-xs text-slate-500 hover:underline" onClick={() => setRunSel(null)}>Close detail</button></div>
              <dl className="mt-2 grid grid-cols-[12rem_1fr] gap-y-1 text-sm">
                <dt className="text-slate-500">Run id</dt><dd className="font-mono text-xs">{selectedRun.id}</dd>
                <dt className="text-slate-500">Stream</dt><dd>{STREAM_LABEL[stream as ReconciliationStreamId]}</dd>
                <dt className="text-slate-500">Result</dt><dd>{statusText(selectedRun.status)}</dd>
                <dt className="text-slate-500">Run at</dt><dd>{when(selectedRun.run_at)} ({formatAge(selectedRun.run_at)} ago)</dd>
                <dt className="text-slate-500">Period</dt><dd>{when(selectedRun.period_start)} to {when(selectedRun.period_end)}</dd>
              </dl>
              {casino ? (
                <p className="mt-2 text-xs text-slate-500">The casino route cannot filter findings by run, so the findings table marks, for the rows loaded, which ones belong to this run.</p>
              ) : (
                <div className="mt-2 flex items-center gap-2">
                  {runOne.error && <ConsoleErrorAlert error={runOne.error} family="recon" op="get" />}
                  <Button variant="secondary" onClick={() => { setRunFilter(selectedRun.id); setMmOffset(0) }} disabled={runFilter === selectedRun.id}>Show only this run&apos;s findings</Button>
                </div>
              )}
            </section>
          )}

          <h2 className="text-sm font-semibold text-slate-900">Findings</h2>
          {runFilter && (
            <p className="flex items-center gap-2 text-sm text-slate-700" role="status">
              Showing only the findings of run <span className="font-mono text-xs">{shortId(runFilter)}</span>.
              <Button variant="secondary" onClick={() => { setRunFilter(null); setMmOffset(0) }}>Show all runs</Button>
            </p>
          )}
          <div className="flex flex-wrap items-end gap-2" role="search" aria-label="Finding filters">
            <label className="flex flex-col gap-1 text-sm"><span className="font-medium text-slate-700">Status</span>
              <Select aria-label="Status" placeholder="Any status" value={status} onChange={(e) => { setStatus(e.target.value); setMmOffset(0) }} options={RECON_STATUSES.map((s) => ({ value: s, label: statusText(s) }))} />
            </label>
            {!casino && stream && (
              <>
                <label className="flex flex-col gap-1 text-sm"><span className="font-medium text-slate-700">Kind</span>
                  <Select aria-label="Kind" placeholder="Any kind" value={kind} onChange={(e) => { setKind(e.target.value); setMmOffset(0) }} options={KINDS_BY_STREAM[stream as StaffStream].map((k) => ({ value: k, label: describeKindLabel(k) }))} />
                </label>
                <TextInput label="Subject id" value={subject} onChange={(e) => { setSubject(e.target.value.trim()); setMmOffset(0) }} autoComplete="off" aria-invalid={!subjectValid} help={subjectValid ? 'Attempt, bet, wallet or transaction id (optional).' : 'Not a valid id (expected a UUID); no request is sent.'} />
              </>
            )}
          </div>
          {mms.error && <ConsoleErrorAlert error={mms.error} family="recon" op="list" />}
          <Table columns={mmColumns} rows={mmRows} getRowKey={(m) => m.id} isLoading={mms.isLoading} emptyMessage="No findings match." pagination={mms.data ? { limit: mms.data.limit, offset: mms.data.offset, total: mms.data.total, onPageChange: setMmOffset } : undefined} />
          {selectedMm && <MismatchDetail m={selectedMm} casino={casino} tenantId={tenantId} stream={stream as ReconciliationStreamId} canOpenAttempt={perms.paymentForceResolutions} onClose={() => setMmSel(null)} />}
        </>
      )}
    </div>
  )
}

function MismatchDetail({ m, casino, tenantId, stream, canOpenAttempt, onClose }: { m: StaffReconMismatch | CasinoReconMismatch; casino: boolean; tenantId: string; stream: ReconciliationStreamId; canOpenAttempt: boolean; onClose: () => void }) {
  // Staff streams: the single-mismatch read is the source of truth; the list row shows while it loads or if it fails.
  const one = useQuery({
    queryKey: ['recon', 'mismatch', stream, tenantId, m.id],
    queryFn: () => getStaffReconMismatch(tenantId, m.id, stream as StaffStream),
    enabled: !casino,
    retry: false,
  })
  const shown = !casino && one.data ? one.data : m
  return (
    <section aria-label="Finding detail" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
      <div className="flex items-start justify-between"><h3 className="text-sm font-semibold">Finding detail</h3><button type="button" className="text-xs text-slate-500 hover:underline" onClick={onClose}>Close detail</button></div>
      {one.error && <ConsoleErrorAlert error={one.error} family="recon" op="get" />}
      {'kind' in shown ? <StaffBody m={shown} tenantId={tenantId} canOpenAttempt={canOpenAttempt} /> : <CasinoBody m={shown} />}
      {!casino && <p className="mt-3 text-xs text-slate-500">Corrections to the ledger go through a governed adjustment (K2) or a force-resolution (K3); this screen only shows the finding.</p>}
    </section>
  )
}

function StaffBody({ m, tenantId, canOpenAttempt }: { m: StaffReconMismatch; tenantId: string; canOpenAttempt: boolean }) {
  const sref = m.subject_ref
  const shows = m.kind === 'pay_amount_mismatch' || m.kind === 'pay_asset_mismatch'
  return (
    <dl className="mt-2 grid grid-cols-[12rem_1fr] gap-y-1 text-sm">
      <dt className="text-slate-500">Finding id</dt><dd className="font-mono text-xs">{m.id}</dd>
      <dt className="text-slate-500">Kind</dt><dd>{describeKindLabel(m.kind)}</dd>
      <dt className="text-slate-500">Status</dt><dd>{statusText(m.status)}</dd>
      <dt className="text-slate-500">Severity</dt><dd>Not provided by the API</dd>
      <dt className="text-slate-500">Detected</dt><dd>{when(m.detected_at)} ({formatAge(m.detected_at)} ago)</dd>
      <dt className="text-slate-500">Run</dt><dd className="font-mono text-xs">{m.run_id}</dd>
      <dt className="text-slate-500">Subject</dt>
      <dd>
        {sref ? (
          <span>
            {Object.prototype.hasOwnProperty.call(SUBJECT_LABEL, sref.type) ? SUBJECT_LABEL[sref.type] : `Unrecognised (${safeCode(sref.type)})`}{' '}
            {sref.type === 'payment_attempt' && canOpenAttempt ? (
              <Link className="font-mono text-xs text-brand-700 underline" to={`/parked-payments?tenant=${encodeURIComponent(tenantId)}&attempt=${encodeURIComponent(sref.id)}`}>{sref.id}</Link>
            ) : (
              <span className="font-mono text-xs">{sref.id}</span>
            )}
          </span>
        ) : (
          'The API names no subject for this finding'
        )}
      </dd>
      <dt className="text-slate-500">Expected</dt><dd>{m.expected_minor_units === null ? 'Not available' : formatMinorUnits(m.expected_minor_units, m.exponent, m.asset_code)}</dd>
      <dt className="text-slate-500">Actual</dt><dd>{m.actual_minor_units === null ? 'Not available' : formatMinorUnits(m.actual_minor_units, m.exponent, m.asset_code)}</dd>
      {!shows && <><dt className="text-slate-500">Amounts</dt><dd className="text-xs text-slate-500">The API returns amounts only for payment amount and asset mismatches.</dd></>}
    </dl>
  )
}

function CasinoBody({ m }: { m: CasinoReconMismatch }) {
  return (
    <>
      <dl className="mt-2 grid grid-cols-[12rem_1fr] gap-y-1 text-sm">
        <dt className="text-slate-500">Finding id</dt><dd className="font-mono text-xs">{m.id}</dd>
        <dt className="text-slate-500">Kind</dt><dd>{describeKindLabel(m.mismatch_kind)}</dd>
        <dt className="text-slate-500">Status</dt><dd>{statusText(m.investigation_status)}</dd>
        <dt className="text-slate-500">Severity</dt><dd>P1 (the API documents every casino finding as P1)</dd>
        <dt className="text-slate-500">Detected</dt><dd>{when(m.created_at)} ({formatAge(m.created_at)} ago)</dd>
        <dt className="text-slate-500">Resolved</dt><dd>{m.resolved_at ? when(m.resolved_at) : 'Not resolved'}</dd>
        <dt className="text-slate-500">Run</dt><dd className="font-mono text-xs">{m.run_id}</dd>
        <dt className="text-slate-500">Key</dt><dd className="break-all font-mono text-xs">{m.reconciliation_key}</dd>
        <dt className="text-slate-500">Expected</dt><dd className="break-all font-mono text-xs">{m.expected_value}</dd>
        <dt className="text-slate-500">Actual</dt><dd className="break-all font-mono text-xs">{m.actual_value}</dd>
      </dl>
      <div className="mt-2"><Hint>Key, expected and actual are stored diagnostic text from the platform and the provider. They are shown as plain text and are never interpreted.</Hint></div>
    </>
  )
}
