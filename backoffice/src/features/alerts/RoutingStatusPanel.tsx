import { useQuery } from '@tanstack/react-query'
import { getAlertingStatus, listAlertRoutes } from '../../api/alerts'
import { Badge } from '../../components/Badge'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { AlertErrorAlert } from './AlertsBits'
import { severityLabel } from './labels'

/**
 * Read-only routing readiness (GET /v1/admin/alerting/status, alert:manage) and route versions
 * (GET /v1/admin/alerting/routes, the separate alert:route_manage). No route authoring in S1. The
 * `missing_operator_input` and `reason` strings from the API are NOT rendered (open text); the booleans
 * and counts are, with fixed sentences.
 */
export function RoutingStatusPanel() {
  const { claims } = useAuth()
  const showRoutes = getNavPermissions(claims?.role).alertRouting
  const status = useQuery({ queryKey: ['alerting-status'], queryFn: getAlertingStatus, retry: false })
  const routes = useQuery({ queryKey: ['alerting-routes'], queryFn: listAlertRoutes, retry: false, enabled: showRoutes })
  return (
    <section aria-label="Routing status" className="rounded-lg border border-border bg-surface p-4 shadow-sm">
      <h2 className="text-sm font-semibold text-slate-900">Routing status (read-only)</h2>
      {status.isLoading && <LoadingSpinner label="Loading routing status..." />}
      {status.error && <AlertErrorAlert error={status.error} op="status" />}
      {status.data && (
        <div className="mt-2 flex flex-col gap-2 text-sm">
          <p>
            Overall routing readiness: <Badge tone={status.data.ready ? 'success' : 'danger'}>{status.data.ready ? 'Ready' : 'Not ready'}</Badge>{' '}
            <span className="text-xs text-slate-500">Configuration only; it never proves a delivery. Evaluated {new Date(status.data.evaluated_at).toLocaleString()}.</span>
          </p>
          <table className="w-full text-left text-xs">
            <thead className="text-slate-500">
              <tr>
                <th className="py-1 pr-3 font-medium">Severity</th>
                <th className="py-1 pr-3 font-medium">Routes</th>
                <th className="py-1 pr-3 font-medium">Enabled</th>
                <th className="py-1 pr-3 font-medium">Reach a person</th>
                <th className="py-1 pr-3 font-medium">Ready</th>
              </tr>
            </thead>
            <tbody>
              {(status.data.severities ?? []).map((s) => (
                <tr key={s.severity} className="border-t border-border">
                  <td className="py-1 pr-3">{severityLabel(s.severity)}</td>
                  <td className="py-1 pr-3">{s.routes_configured}</td>
                  <td className="py-1 pr-3">{s.routes_enabled}</td>
                  <td className="py-1 pr-3">{s.human_notification_routes}</td>
                  <td className="py-1 pr-3">{s.ready ? 'Yes' : 'No'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {showRoutes && routes.error && <AlertErrorAlert error={routes.error} op="routes" />}
      {routes.data && (
        <div className="mt-3 text-xs">
          <p className="font-medium text-slate-700">Current route versions</p>
          {routes.data.routes.length === 0 ? (
            <p className="text-slate-500">No routes are configured.</p>
          ) : (
            <ul className="mt-1 list-disc pl-5 text-slate-600">
              {routes.data.routes.map((r) => (
                <li key={r.id}>
                  {severityLabel(r.severity)} step {r.escalation_step}: {r.channel_kind === 'log' ? 'log' : r.channel_kind === 'mock' ? 'mock' : 'unknown'} channel,{' '}
                  {r.enabled ? 'enabled' : 'disabled'}, {r.human_notification ? 'reaches a person' : 'notifies nobody'}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </section>
  )
}
