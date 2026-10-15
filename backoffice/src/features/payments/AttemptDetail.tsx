import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { listForceResolutions } from '../../api/forceResolutions'
import { getPaymentAttempt } from '../../api/paymentAttempts'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { formatAge, formatMinorUnits } from '../../lib/minorUnits'
import { ConsoleErrorAlert, Hint, shortId } from '../consoles/ConsoleBits'
import { EnumText } from '../killswitch/KillSwitchBits'
import { safeCode } from '../killswitch/labels'
import { ADMISSIBLE_HINT, CONSOLE_KINDS, describeKind } from './labels'
import { attemptStateTone, describeAttemptState, describeOperation, describeReason } from './labels'

interface TimelineEntry {
  at: string
  text: string
}

function when(iso: string): string {
  const t = Date.parse(iso)
  return Number.isNaN(t) ? 'Unknown time' : new Date(t).toLocaleString()
}

/**
 * One attempt (R1 single read) with a timeline assembled ONLY from fields the API returns: created, last change,
 * resolved, and the K3 resolutions filed for this attempt (when the viewer may read them). The API has no event
 * history route for attempts, so intermediate provider interactions are not shown and the screen says so.
 * Never rendered: the provider reference text (the API does not return it), merchant reference, idempotency key,
 * player, wallet or destination (the API does not return them either).
 */
export function AttemptDetail({ tenantId, attemptId, onClose }: { tenantId: string; attemptId: string; onClose: () => void }) {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const q = useQuery({ queryKey: ['parked', 'detail', tenantId, attemptId], queryFn: () => getPaymentAttempt(tenantId, attemptId), retry: false })
  const res = useQuery({
    queryKey: ['parked', 'resolutions', tenantId],
    queryFn: () => listForceResolutions(tenantId),
    enabled: perms.paymentForceResolutions,
    retry: false,
  })
  const a = q.data
  const mine = (res.data?.items ?? []).filter((r) => r.attempt_id === attemptId)
  const pendingMine = mine.find((r) => r.state === 'pending')

  const timeline: TimelineEntry[] = []
  if (a) {
    timeline.push({ at: a.created_at, text: `Attempt ${a.attempt_no} created` })
    if (a.updated_at !== a.created_at) timeline.push({ at: a.updated_at, text: `Last change: ${describeAttemptState(a.state).label}` })
    if (a.resolved_at) timeline.push({ at: a.resolved_at, text: 'Left the in-flight states' })
  }
  for (const r of mine) {
    timeline.push({ at: r.created_at, text: `Force-resolution filed: ${describeKind(r.kind).label} (${safeCode(r.state)})` })
  }
  timeline.sort((x, y) => Date.parse(x.at) - Date.parse(y.at))

  const offeredKinds = (a?.admissible_kinds ?? []).filter((k) => (CONSOLE_KINDS as readonly string[]).includes(k))
  const m4Only = (a?.admissible_kinds ?? []).filter((k) => k.startsWith('m4_'))

  return (
    <section aria-label="Attempt detail" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
      <div className="flex items-start justify-between gap-2">
        <h2 className="text-sm font-semibold text-slate-900">Attempt detail</h2>
        <button type="button" className="text-xs text-slate-500 hover:underline" onClick={onClose}>Close detail</button>
      </div>
      {q.isLoading && <LoadingSpinner label="Loading attempt..." />}
      {q.error && <ConsoleErrorAlert error={q.error} family="attempts" op="get" />}
      {a && (
        <div className="mt-3 flex flex-col gap-4">
          <dl className="grid grid-cols-[13rem_1fr] gap-y-1 text-sm">
            <dt className="text-slate-500">Attempt id</dt>
            <dd className="font-mono text-xs">{a.id}</dd>
            <dt className="text-slate-500">Operation</dt>
            <dd><EnumText view={describeOperation(a.operation)} /> (attempt {a.attempt_no})</dd>
            <dt className="text-slate-500">State</dt>
            <dd><Badge tone={attemptStateTone(a.state)}><EnumText view={describeAttemptState(a.state)} /></Badge></dd>
            <dt className="text-slate-500">Park reason</dt>
            <dd><EnumText view={describeReason(a.terminal_reason)} /></dd>
            <dt className="text-slate-500">Amount</dt>
            <dd>{formatMinorUnits(a.amount_minor_units, a.exponent, a.asset_code)}</dd>
            <dt className="text-slate-500">Provider</dt>
            <dd className="font-mono text-xs">{a.provider_id ? safeCode(a.provider_id) : 'None'}</dd>
            <dt className="text-slate-500">Provider reference held</dt>
            <dd>{a.has_provider_reference ? 'Yes (the reference text is never shown here)' : 'No'}</dd>
            <dt className="text-slate-500">Subject</dt>
            <dd>
              {a.subject_ref.withdrawal_request_id ? (
                <span>
                  Withdrawal{' '}
                  {perms.withdrawals ? (
                    <Link className="font-mono text-xs text-brand-700 underline" to={`/withdrawals/${encodeURIComponent(a.subject_ref.withdrawal_request_id)}`}>{a.subject_ref.withdrawal_request_id}</Link>
                  ) : (
                    <span className="font-mono text-xs">{a.subject_ref.withdrawal_request_id}</span>
                  )}
                </span>
              ) : a.subject_ref.deposit_intent_id ? (
                <span>Deposit intent <span className="font-mono text-xs">{a.subject_ref.deposit_intent_id}</span></span>
              ) : (
                'Not available'
              )}
            </dd>
            <dt className="text-slate-500">Age</dt>
            <dd>{formatAge(a.created_at)} since created</dd>
            <dt className="text-slate-500">Open resolution</dt>
            <dd>
              {a.open_resolution_id ? <span className="font-mono text-xs">{a.open_resolution_id}</span> : pendingMine ? <span>Pending (<span className="font-mono text-xs">{shortId(pendingMine.id)}</span>)</span> : 'None visible'}
            </dd>
          </dl>

          <div>
            <h3 className="text-sm font-semibold text-slate-900">Timeline</h3>
            <ol className="mt-2 flex flex-col gap-1 text-sm" aria-label="Timeline">
              {timeline.map((t, i) => (
                <li key={`${t.at}-${i}`}><span className="text-slate-500">{when(t.at)}</span> - {t.text}</li>
              ))}
            </ol>
            <p className="mt-2 text-xs text-slate-500">
              Built only from the fields the API returns. The API has no event history for an attempt, so provider interactions between these points are not shown.
              {perms.paymentForceResolutions ? '' : ' Your role does not read force-resolutions, so none are listed.'}
            </p>
            {res.error && <ConsoleErrorAlert error={res.error} family="k3" op="list" />}
          </div>

          <div>
            <h3 className="text-sm font-semibold text-slate-900">Resolution options (server hint)</h3>
            {a.admissible_kinds.length === 0 ? (
              <p className="mt-1 text-sm text-slate-600">The server currently admits no force-resolution kind for this attempt.</p>
            ) : (
              <ul className="mt-1 list-disc pl-5 text-sm">
                {a.admissible_kinds.map((k) => <li key={k}><EnumText view={describeKind(k)} /></li>)}
              </ul>
            )}
            <p className="mt-1 text-xs text-slate-500">{ADMISSIBLE_HINT}</p>
            {m4Only.length > 0 && <p className="mt-1 text-xs text-slate-500">M4 kinds are not offered by this console (separate slice, MOCK only).</p>}
            {perms.paymentForceResolutionAct && offeredKinds.length > 0 && !pendingMine && (
              <div className="mt-2">
                <Link to={`/force-resolutions?tenant=${encodeURIComponent(tenantId)}&attempt=${encodeURIComponent(a.id)}`}>
                  <Button variant="secondary">Propose a force-resolution</Button>
                </Link>
              </div>
            )}
            {pendingMine && perms.paymentForceResolutions && (
              <div className="mt-2 flex flex-col gap-2">
                <Hint>A force-resolution for this attempt is already pending. It must be decided or cancelled before another is proposed.</Hint>
                <Link to={`/force-resolutions?tenant=${encodeURIComponent(tenantId)}&resolution=${encodeURIComponent(pendingMine.id)}`}>
                  <Button variant="secondary">Open the pending force-resolution</Button>
                </Link>
              </div>
            )}
            {!perms.paymentForceResolutionAct && <p className="mt-2 text-xs text-slate-500">Your role is read only for force-resolutions.</p>}
          </div>
        </div>
      )}
    </section>
  )
}
