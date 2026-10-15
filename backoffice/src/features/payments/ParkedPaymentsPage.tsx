import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { ATTEMPT_OPERATIONS, ATTEMPT_STATES, TERMINAL_REASONS, listPaymentAttempts, type AttemptOperation, type AttemptState, type PaymentAttempt } from '../../api/paymentAttempts'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { formatAge, formatMinorUnits } from '../../lib/minorUnits'
import { ConsoleErrorAlert, MockPaymentsBanner, isUuid, useConsoleTenant } from '../consoles/ConsoleBits'
import { EnumText } from '../killswitch/KillSwitchBits'
import { safeCode } from '../killswitch/labels'
import { AttemptDetail } from './AttemptDetail'
import { attemptStateTone, describeAttemptState, describeOperation, describeReason } from './labels'

const PAGE_SIZE = 50

/**
 * Parked payments queue (ADR 0114 S3, R1). Read-only. The queue is the attempts list filtered to a state (default
 * `disputed`, the parked state). Pagination is the API's opaque keyset cursor: Next uses the page's `next_cursor`,
 * Previous pops a stack of cursors this screen already used. Filters the ADR names that the API does NOT have
 * (provider, age) are not offered: a client-side filter over one page would silently hide rows. Age is shown per
 * row instead. Route gating by permissions is a convenience; the server decides and every refusal is rendered.
 */
export function ParkedPaymentsPage() {
  const { platform, tenantId, picker, searchParams, setSearchParams } = useConsoleTenant()
  const [state, setState] = useState<AttemptState | ''>('disputed')
  const [operation, setOperation] = useState<AttemptOperation | ''>('')
  const [reason, setReason] = useState('')
  const [withdrawalId, setWithdrawalId] = useState('')
  // cursors[i] is the cursor that produced page i; page 0 has none.
  const [cursors, setCursors] = useState<string[]>([])
  const cursor = cursors.length > 0 ? cursors[cursors.length - 1] : undefined
  const withdrawalValid = withdrawalId === '' || isUuid(withdrawalId)
  const selectedId = searchParams.get('attempt') ?? ''

  const list = useQuery({
    queryKey: ['parked', 'list', tenantId, state, operation, reason, withdrawalValid ? withdrawalId : '', cursor ?? ''],
    queryFn: () =>
      listPaymentAttempts(tenantId, {
        state: state || undefined,
        operation: operation || undefined,
        terminal_reason: reason || undefined,
        withdrawal_request_id: withdrawalId && withdrawalValid ? withdrawalId : undefined,
        limit: PAGE_SIZE,
        cursor,
      }),
    enabled: tenantId !== '' && withdrawalValid,
    retry: false,
  })

  function resetPaging() {
    setCursors([])
  }
  function select(id: string) {
    const next = new URLSearchParams(searchParams)
    if (id) next.set('attempt', id)
    else next.delete('attempt')
    setSearchParams(next, { replace: true })
  }
  const rows = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Parked payments" description="Payment attempts the platform could not settle by itself, with the reason each one is parked. Read only." />
      <MockPaymentsBanner />
      {picker}
      {tenantId === '' && platform && <p className="text-sm text-slate-500">Choose a tenant to see its payment attempts.</p>}
      {tenantId !== '' && (
        <>
          <div className="flex flex-wrap items-end gap-2" role="search" aria-label="Attempt filters">
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium text-slate-700">State</span>
              <Select aria-label="State" placeholder="Any state" value={state} onChange={(e) => { setState(e.target.value as AttemptState | ''); resetPaging() }} options={ATTEMPT_STATES.map((s) => ({ value: s, label: describeAttemptState(s).label }))} />
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium text-slate-700">Operation</span>
              <Select aria-label="Operation" placeholder="Any operation" value={operation} onChange={(e) => { setOperation(e.target.value as AttemptOperation | ''); resetPaging() }} options={ATTEMPT_OPERATIONS.map((s) => ({ value: s, label: describeOperation(s).label }))} />
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium text-slate-700">Park reason</span>
              <Select aria-label="Park reason" placeholder="Any reason" value={reason} onChange={(e) => { setReason(e.target.value); resetPaging() }} options={TERMINAL_REASONS.map((s) => ({ value: s, label: describeReason(s).label }))} />
            </label>
            <TextInput
              label="Withdrawal id"
              value={withdrawalId}
              onChange={(e) => { setWithdrawalId(e.target.value.trim()); resetPaging() }}
              autoComplete="off"
              aria-invalid={!withdrawalValid}
              help={withdrawalValid ? 'Exact withdrawal request id (optional).' : 'Not a valid id (expected a UUID); no request is sent.'}
            />
            <Button variant="secondary" onClick={() => void list.refetch()} isLoading={list.isFetching}>Refresh</Button>
          </div>
          <p className="text-xs text-slate-500">
            The API filters by state, operation, park reason and withdrawal only. It has no provider or age filter, so none is offered; the Age column is computed from the creation time.
          </p>
          {list.isLoading && <LoadingSpinner label="Loading payment attempts..." />}
          {list.error && <ConsoleErrorAlert error={list.error} family="attempts" op="list" />}
          {list.data && rows.length === 0 && <p className="text-sm text-slate-600">No payment attempts match these filters.</p>}
          {rows.length > 0 && (
            <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
              <table className="w-full text-left text-sm">
                <caption className="sr-only">Payment attempts</caption>
                <thead className="bg-surface-alt text-xs uppercase tracking-wide text-slate-500">
                  <tr>
                    {['Age', 'Operation', 'State', 'Park reason', 'Amount', 'Provider', 'Provider reference held', 'Open resolution', ''].map((h) => (
                      <th key={h || 'actions'} scope="col" className="px-3 py-2 font-medium">{h}</th>
                    ))}
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {rows.map((a) => (
                    <AttemptRow key={a.id} a={a} selected={a.id === selectedId} onOpen={() => select(a.id)} />
                  ))}
                </tbody>
              </table>
              <div className="flex items-center justify-between border-t border-border px-4 py-3 text-sm text-slate-600">
                <span>Page {cursors.length + 1} ({rows.length} shown, newest first)</span>
                <div className="flex gap-2">
                  <Button variant="secondary" disabled={cursors.length === 0} onClick={() => setCursors((c) => c.slice(0, -1))}>Previous</Button>
                  <Button variant="secondary" disabled={!list.data?.next_cursor} onClick={() => { const n = list.data?.next_cursor; if (n) setCursors((c) => [...c, n]) }}>Next</Button>
                </div>
              </div>
            </div>
          )}
          {isUuid(selectedId) && <AttemptDetail tenantId={tenantId} attemptId={selectedId} onClose={() => select('')} />}
        </>
      )}
    </div>
  )
}

function AttemptRow({ a, selected, onOpen }: { a: PaymentAttempt; selected: boolean; onOpen: () => void }) {
  return (
    <tr className={selected ? 'bg-brand-50' : undefined}>
      <td className="px-3 py-2">{formatAge(a.created_at)}</td>
      <td className="px-3 py-2"><EnumText view={describeOperation(a.operation)} /></td>
      <td className="px-3 py-2">
        <Badge tone={attemptStateTone(a.state)}><EnumText view={describeAttemptState(a.state)} /></Badge>
      </td>
      <td className="px-3 py-2"><EnumText view={describeReason(a.terminal_reason)} /></td>
      <td className="px-3 py-2 whitespace-nowrap">{formatMinorUnits(a.amount_minor_units, a.exponent, a.asset_code)}</td>
      <td className="px-3 py-2 font-mono text-xs">{a.provider_id ? safeCode(a.provider_id) : 'None'}</td>
      <td className="px-3 py-2">{a.has_provider_reference ? 'Yes' : 'No'}</td>
      <td className="px-3 py-2">{a.open_resolution_id ? 'Yes' : 'None visible'}</td>
      <td className="px-3 py-2">
        <Button variant="secondary" onClick={onOpen} aria-label={`Open attempt ${a.id.slice(0, 8)}`}>Details</Button>
      </td>
    </tr>
  )
}
