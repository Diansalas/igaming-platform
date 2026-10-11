import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { listAlerts, type AlertSeverity, type AlertState, type PlatformAlert } from '../../api/alerts'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { AlertErrorAlert, DeliveryBanner, KindText, TransitionDialog } from './AlertsBits'
import { RoutingStatusPanel } from './RoutingStatusPanel'
import {
  LIMIT_OPTIONS,
  SEVERITY_OPTIONS,
  STATE_OPTIONS,
  deliveryLabel,
  describeKind,
  severityLabel,
  severityTone,
  stateLabel,
  stateTone,
  unroutedLabel,
} from './labels'

const STATES = new Set(['open', 'acked', 'resolved'])
const SEVERITIES = new Set(['p1', 'p2', 'p3'])

/**
 * Platform alerts console (ADR 0114 S1). Route-gated by `alertManage` (platform_admin) as a convenience
 * only; the server is authoritative and every refusal is rendered through the closed code map.
 * The list API supports state, severity and limit only: no kind filter, no offset/cursor, no detail GET.
 */
export function AlertsPage() {
  const qc = useQueryClient()
  const [state, setState] = useState('open')
  const [severity, setSeverity] = useState('')
  const [limit, setLimit] = useState('50')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [dialog, setDialog] = useState<{ op: 'ack' | 'resolve'; alert: PlatformAlert } | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const query = useQuery({
    queryKey: ['alerts', state, severity, limit],
    queryFn: () =>
      listAlerts({
        state: STATES.has(state) ? (state as AlertState) : undefined,
        severity: SEVERITIES.has(severity) ? (severity as AlertSeverity) : undefined,
        limit: Number(limit),
      }),
    retry: false,
  })
  const alerts = query.data?.alerts ?? []
  const selected = alerts.find((a) => a.id === selectedId) ?? null
  const mayHaveMore = query.data !== undefined && alerts.length >= Number(limit)

  function pickFilter(set: (v: string) => void) {
    return (e: { target: { value: string } }) => {
      setNotice(null)
      set(e.target.value)
    }
  }

  function refresh() {
    void qc.invalidateQueries({ queryKey: ['alerts'] })
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title="Alerts" description="Platform alerts, newest first by first-seen time." />
      <DeliveryBanner />
      <RoutingStatusPanel />
      <div className="flex flex-wrap items-end gap-2">
        <Select aria-label="Alert state" placeholder="All states" options={STATE_OPTIONS} value={state} onChange={pickFilter(setState)} />
        <Select aria-label="Alert severity" placeholder="All severities" options={SEVERITY_OPTIONS} value={severity} onChange={pickFilter(setSeverity)} />
        <Select aria-label="Rows to load" options={LIMIT_OPTIONS} value={limit} onChange={pickFilter(setLimit)} />
        <Button variant="secondary" onClick={() => { setNotice(null); setSeverity('p1') }}>
          Show P1 only
        </Button>
        <Button variant="secondary" onClick={() => { setNotice(null); refresh() }} isLoading={query.isFetching}>
          Refresh
        </Button>
      </div>
      <p className="text-xs text-slate-500">
        The API filters by state and severity only. There is no filter by kind, and no way to page past the newest 200 alerts.
      </p>
      {notice && (
        <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">
          {notice}
        </p>
      )}
      {query.isLoading && <LoadingSpinner label="Loading alerts..." />}
      {query.error && <AlertErrorAlert error={query.error} op="list" />}
      {query.data && alerts.length === 0 && <p className="text-sm text-slate-500">No alerts match these filters.</p>}
      {alerts.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-border bg-surface shadow-sm">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Platform alerts</caption>
            <thead className="bg-surface-alt text-xs text-slate-500">
              <tr>
                <th className="px-3 py-2 font-medium">Alert</th>
                <th className="px-3 py-2 font-medium">Severity</th>
                <th className="px-3 py-2 font-medium">State</th>
                <th className="px-3 py-2 font-medium">Delivery</th>
                <th className="px-3 py-2 font-medium">First seen</th>
              </tr>
            </thead>
            <tbody>
              {alerts.map((a) => (
                <tr key={a.id} className={`border-t border-border hover:bg-surface-alt ${a.id === selectedId ? 'bg-surface-alt' : ''}`}>
                  <td className="px-3 py-2">
                    <button type="button" className="text-left text-brand-700 hover:underline" onClick={() => setSelectedId(a.id)}>
                      <KindText kind={a.kind} />
                    </button>
                  </td>
                  <td className="px-3 py-2">
                    <Badge tone={severityTone(a.severity)}>{severityLabel(a.severity)}</Badge>
                  </td>
                  <td className="px-3 py-2">
                    <Badge tone={stateTone(a.state)}>{stateLabel(a.state)}</Badge>
                  </td>
                  <td className="px-3 py-2 text-xs">{a.notified_a_person ? 'A person was notified' : 'Nobody was notified'}</td>
                  <td className="px-3 py-2">{new Date(a.first_seen_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {mayHaveMore && (
        <p className="text-xs text-slate-500">
          Showing the newest {limit}. There may be more alerts that match. The list is sorted by first-seen time only, so up to 200 newer P3 alerts can push an open P1 out of view. Use &quot;Show P1 only&quot; or filter by severity, or raise the row count (maximum 200).
        </p>
      )}
      {selected && <AlertDetail alert={selected} onAct={(op) => { setNotice(null); setDialog({ op, alert: selected }) }} onClose={() => setSelectedId(null)} />}
      {dialog && (
        <TransitionDialog
          op={dialog.op}
          alert={dialog.alert}
          onClose={() => setDialog(null)}
          onRefused={() => { setNotice(null); refresh() }}
          onDone={(r) => {
            setDialog(null)
            // HARDCODED while ALERT-DELIVERY-1 is OPEN: derive from /alerting/status human_notification_routes when it closes.
            setNotice(`Alert ${r.state === 'resolved' ? 'resolved' : 'acknowledged'}. Nobody was notified: delivery is not configured.`)
            refresh()
          }}
        />
      )}
    </div>
  )
}

function AlertDetail({ alert, onAct, onClose }: { alert: PlatformAlert; onAct: (op: 'ack' | 'resolve') => void; onClose: () => void }) {
  const k = describeKind(alert.kind)
  const unrouted = unroutedLabel(alert.unrouted_reason)
  const ref = useRef<HTMLElement>(null)
  useEffect(() => {
    ref.current?.focus()
  }, [alert.id])
  return (
    <section ref={ref} tabIndex={-1} aria-label="Alert detail" className="rounded-lg focus:outline-none focus:ring-2 focus:ring-brand-500 border border-border bg-surface p-4 shadow-sm">
      <div className="flex items-start justify-between gap-2">
        <h2 className="text-sm font-semibold text-slate-900">{k.title}</h2>
        <button type="button" className="text-xs text-slate-500 hover:underline" onClick={onClose}>
          Close detail
        </button>
      </div>
      <p className="mt-1 text-sm text-slate-600">{k.description}</p>
      <dl className="mt-3 grid grid-cols-[10rem_1fr] gap-y-1 text-sm">
        <dt className="text-slate-500">Kind code</dt>
        <dd className="font-mono text-xs">{k.code}</dd>
        <dt className="text-slate-500">Alert ID</dt>
        <dd className="font-mono text-xs">{alert.id}</dd>
        <dt className="text-slate-500">Severity</dt>
        <dd>{severityLabel(alert.severity)}</dd>
        <dt className="text-slate-500">State</dt>
        <dd>{stateLabel(alert.state)}</dd>
        <dt className="text-slate-500">Subject tenant</dt>
        <dd className="font-mono text-xs">{alert.subject_tenant_id ?? 'None (platform-wide)'}</dd>
        <dt className="text-slate-500">First seen</dt>
        <dd>{new Date(alert.first_seen_at).toLocaleString()}</dd>
        <dt className="text-slate-500">Delivery</dt>
        <dd>
          {deliveryLabel(alert.delivery_state)}
          {unrouted ? `: ${unrouted}` : ''}. {alert.notified_a_person ? 'A person was notified.' : 'Nobody was notified.'}
        </dd>
      </dl>
      {/* Actions are shown for every open/acked alert. The page itself is already gated by alertManage (the same
          permission ack/resolve need), so there is no separate hide-actions case; the server decides. */}
      {(alert.state === 'open' || alert.state === 'acked') && (
        <div className="mt-3 flex gap-2">
          {alert.state === 'open' && (
            <Button variant="secondary" onClick={() => onAct('ack')}>
              Acknowledge
            </Button>
          )}
          <Button onClick={() => onAct('resolve')}>Resolve</Button>
        </div>
      )}
    </section>
  )
}
