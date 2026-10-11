// ADR 0114 S1 / ADR 0102 alerting client. Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (/v1/admin/alerts, /ack, /resolve, /v1/admin/alerting/status, /v1/admin/alerting/routes); no field or
// route is invented. The list route supports ONLY the `state`, `severity` and `limit` (1..200) query
// parameters: there is no kind filter, no offset/cursor and no single-alert GET (API gaps, see the S1 report).
import { apiFetch, buildQuery } from './client'

export type AlertState = 'open' | 'acked' | 'resolved'
export type AlertSeverity = 'p1' | 'p2' | 'p3'
export type AlertDeliveryState = 'pending' | 'retrying' | 'unrouted' | 'delivery_failed' | 'suppressed' | 'recorded_non_human' | 'delivered'
export type AlertUnroutedReason = 'no_route' | 'channel_disabled' | 'no_sink'

export const ALERT_MAX_LIMIT = 200

export interface PlatformAlert {
  id: string
  /** Open string on the wire; rendered through the kind catalogue, never as raw text. */
  kind: string
  severity: AlertSeverity
  state: AlertState
  subject_tenant_id?: string | null
  first_seen_at: string
  delivery_state: AlertDeliveryState
  unrouted_reason?: AlertUnroutedReason | null
  notified_a_person: boolean
}

export interface AlertListParams {
  state?: AlertState
  severity?: AlertSeverity
  limit?: number
}

export function listAlerts(params: AlertListParams): Promise<{ alerts: PlatformAlert[] }> {
  return apiFetch(`/v1/admin/alerts${buildQuery(params)}`)
}

/** The transition response ("The alert after the transition"); the handler returns these fields only. */
export interface AlertTransitionResult {
  id: string
  kind: string
  severity: AlertSeverity
  state: AlertState
  acked_at?: string | null
  resolved_at?: string | null
}

/** ack: the body is optional; reason_code, when sent, is audit metadata only. */
export function ackAlert(id: string, reasonCode?: string): Promise<AlertTransitionResult> {
  return apiFetch(`/v1/admin/alerts/${encodeURIComponent(id)}/ack`, {
    method: 'POST',
    body: reasonCode ? JSON.stringify({ reason_code: reasonCode }) : undefined,
  })
}

/** resolve: reason_code is required (^[a-z0-9][a-z0-9_.-]{0,63}$). */
export function resolveAlert(id: string, reasonCode: string): Promise<AlertTransitionResult> {
  return apiFetch(`/v1/admin/alerts/${encodeURIComponent(id)}/resolve`, {
    method: 'POST',
    body: JSON.stringify({ reason_code: reasonCode }),
  })
}

/** The server's own reason_code pattern, mirrored as a hint only; the server is authoritative. */
export const ALERT_REASON_CODE_PATTERN = /^[a-z0-9][a-z0-9_.-]{0,63}$/

export interface AlertingSeverityStatus {
  severity: AlertSeverity
  required_for_readiness: boolean
  routes_configured: number
  routes_enabled: number
  human_notification_routes: number
  ready: boolean
  reason: string
  missing_operator_input?: string
}

export interface AlertingStatus {
  ready: boolean
  evaluated_at: string
  note?: string
  severities: AlertingSeverityStatus[]
}

export function getAlertingStatus(): Promise<AlertingStatus> {
  return apiFetch('/v1/admin/alerting/status')
}

export interface AlertRoute {
  id: string
  severity: AlertSeverity
  escalation_step: number
  channel_kind: 'log' | 'mock'
  enabled: boolean
  human_notification: boolean
  effective_from: string
  superseded_at?: string | null
}

/** Requires alert:route_manage (a separate permission); read-only here, no route authoring in S1. */
export function listAlertRoutes(): Promise<{ routes: AlertRoute[] }> {
  return apiFetch('/v1/admin/alerting/routes')
}
