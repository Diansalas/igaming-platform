import type { ReactNode } from 'react'
import type { LaunchReadiness } from '../../api/launch'
import { Badge } from '../../components/Badge'
import { describeLaunchError } from './errors'
import {
  FUNDS_INACCESSIBLE_NOTICE,
  FUNDS_OMISSIONS_NOTICE,
  FUNDS_STRANDED_NOTICE,
  LAUNCH_DISCLAIMER,
  LEGACY_BASELINE_LABEL,
  checkNote,
  checkTitle,
  groupDecimalString,
  subjectStatusLabel,
  subjectStatusTone,
} from './labels'

/**
 * The fixed disclaimer. Rendered on every screen that shows licensing, readiness or decision data.
 * The UI sentence is always present; the API's own disclaimer string (every 2xx carries one) is
 * shown as well when it differs.
 */
export function LaunchDisclaimer({ apiDisclaimer }: { apiDisclaimer?: string }) {
  return (
    <div role="note" aria-label="Disclaimer" className="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900">
      <p className="font-medium">{LAUNCH_DISCLAIMER}</p>
      {apiDisclaimer && apiDisclaimer !== LAUNCH_DISCLAIMER && <p className="mt-1 text-xs">{apiDisclaimer}</p>}
    </div>
  )
}

/** Status badge covering pending_launch / active / suspended / closed (never an unlabeled unknown). */
export function LaunchStatusBadge({ status }: { status: string }) {
  return <Badge tone={subjectStatusTone(status)}>{subjectStatusLabel(status)}</Badge>
}

export function LegacyBaselineBadge() {
  return <Badge tone="neutral">{LEGACY_BASELINE_LABEL}</Badge>
}

export function WarningBox({ children }: { children: ReactNode }) {
  return (
    <div role="alert" className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs font-medium text-amber-900">
      <span className="font-bold">WARNING: </span>
      {children}
    </div>
  )
}

/** Error banner built from fixed UI text + closed codes only; the server message is never rendered. */
export function LaunchErrorAlert({ error }: { error: unknown }) {
  if (!error) return null
  const v = describeLaunchError(error)
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{v.message}</p>
      {v.checkCodes.length > 0 && (
        <ul className="mt-2 list-disc pl-5 text-xs">
          {v.checkCodes.map((c) => (
            <li key={c.code}>
              <span className="font-mono">{c.code}</span> - {c.text}
            </li>
          ))}
        </ul>
      )}
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {v.requestId}</p>}
    </div>
  )
}

/** Who did it: an id if the API gave one, else only the scope (a tenant never sees platform staff ids). */
export function actorLabel(id: string | undefined, scope: string | undefined): string {
  if (id) return `Staff ${id.slice(0, 8)}`
  if (scope === 'platform') return 'Platform staff'
  if (scope === 'tenant') return 'Tenant staff'
  if (scope === 'system') return 'System'
  return 'Not shown'
}

const RESULT_VIEW = {
  pass: { label: 'Pass', tone: 'success' as const },
  fail: { label: 'Fail', tone: 'danger' as const },
  not_evaluable: { label: 'Info: not evaluable', tone: 'info' as const },
}

function detailText(detail: string): string {
  if (detail === 'see_platform') return 'Visible to platform staff only'
  return detail.replace(/_/g, ' ')
}

export interface FundsViewProps {
  readiness: LaunchReadiness
  /** Which fixed notice to add on top of the API notices. */
  mode: 'close' | 'suspend' | 'other'
  /** The caller already shows the fixed close/suspend notice elsewhere in the same dialog. */
  hideModeNotice?: boolean
}

/** R-PLAYER-FUNDS display data. Balances are DECIMAL STRINGS and are only ever treated as strings. */
export function FundsView({ readiness, mode, hideModeNotice }: FundsViewProps) {
  const balances = Object.entries(readiness.balances ?? {})
  const counts = Object.entries(readiness.counts ?? {})
  if (balances.length === 0 && counts.length === 0 && mode === 'other') return null
  return (
    <div className="flex flex-col gap-2 rounded-md border border-border bg-surface-alt p-3 text-sm" aria-label="Player funds">
      <h4 className="font-semibold text-slate-800">Player funds (live, informational)</h4>
      {balances.length > 0 ? (
        <table className="text-xs">
          <thead>
            <tr className="text-left text-slate-500">
              <th className="pr-4 font-medium">Asset</th>
              <th className="font-medium">Sum (minor units)</th>
            </tr>
          </thead>
          <tbody>
            {balances.map(([asset, sum]) => (
              <tr key={asset}>
                <td className="pr-4 font-medium">{asset}</td>
                <td className="font-mono" data-testid={`balance-${asset}`} title={sum}>
                  {groupDecimalString(sum)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="text-xs text-slate-500">No per-asset sums reported.</p>
      )}
      {counts.length > 0 && (
        <ul className="text-xs text-slate-600">
          {counts.map(([k, n]) => (
            <li key={k}>
              {k.replace(/_/g, ' ')}: <span className="font-mono">{String(n)}</span>
            </li>
          ))}
        </ul>
      )}
      {mode === 'close' && !hideModeNotice && <p className="text-xs font-medium text-red-700">{FUNDS_STRANDED_NOTICE}</p>}
      {mode === 'suspend' && !hideModeNotice && <p className="text-xs font-medium text-amber-800">{FUNDS_INACCESSIBLE_NOTICE}</p>}
      {mode !== 'other' && <p className="text-xs text-slate-500">{FUNDS_OMISSIONS_NOTICE}</p>}
      {(readiness.notices ?? []).map((n) => (
        <p key={n} className="text-xs text-slate-600">
          {n}
        </p>
      ))}
    </div>
  )
}

export interface ReadinessChecklistProps {
  readiness: LaunchReadiness
  mode?: 'close' | 'suspend' | 'other'
  /** Present on the request form: tick each non-passing check. Absent: read-only display. */
  acknowledged?: Set<string>
  onToggleAck?: (code: string, checked: boolean) => void
}

/** The derived readiness checks with pass/fail/info and acknowledgement checkboxes by check code. */
export function ReadinessChecklist({ readiness, mode = 'other', acknowledged, onToggleAck }: ReadinessChecklistProps) {
  const nonPassing = new Set(readiness.non_passing_check_codes)
  const env = readiness.environment === 'redacted' ? 'Not shown to tenant staff' : readiness.environment
  return (
    <div className="flex flex-col gap-3" aria-label="Readiness checklist">
      <p className="text-xs text-slate-500">
        Informational only. Readiness shows that configuration is present, not that it is adequate, and a non-passing check never
        blocks by itself but must be acknowledged. Environment: {env}.
      </p>
      <ul className="flex flex-col gap-2">
        {readiness.checks.map((c) => {
          const view = RESULT_VIEW[c.result] ?? { label: `Info: ${c.result}`, tone: 'info' as const }
          const needsAck = nonPassing.has(c.code)
          return (
            <li key={c.code} className="flex flex-col gap-1 rounded-md border border-border px-3 py-2 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <Badge tone={view.tone}>{view.label}</Badge>
                <span className="font-mono text-xs text-slate-500">{c.code}</span>
                <span className="font-medium text-slate-800">{checkTitle(c.code)}</span>
              </div>
              {c.detail_code && <p className="text-xs text-slate-500">{detailText(c.detail_code)}</p>}
              {needsAck && checkNote(c.code) && <p className="text-xs text-slate-600">{checkNote(c.code)}</p>}
              {needsAck && c.code === 'R-RISK-RULES-LEGACY' && (
                <ul className="text-xs text-slate-600" aria-label="Legacy risk rule ids">
                  {Object.entries(readiness.refs ?? {})
                    .filter(([k]) => k.startsWith('risk_rules_legacy'))
                    .flatMap(([k, ids]) =>
                    ids.map((id) => (
                      <li key={`${k}-${id}`}>
                        {k.replace(/_/g, ' ')}: <span className="font-mono">{id}</span>
                      </li>
                    )),
                  )}
                </ul>
              )}
              {needsAck && c.code === 'R-OPERATING-BASIS' && (
                <ul className="text-xs text-slate-600" aria-label="Operating bases due for review">
                  {Object.entries(readiness.refs ?? {})
                    .filter(([k]) => k.startsWith('operating_basis_review'))
                    .flatMap(([k, ids]) =>
                      ids.map((id) => (
                        <li key={`${k}-${id}`}>
                          {k.replace(/_/g, ' ')}: <span className="font-mono">{id}</span>
                        </li>
                      )),
                    )}
                </ul>
              )}
              {needsAck && acknowledged && onToggleAck && (
                <label className="flex items-start gap-2 text-xs text-slate-700">
                  <input
                    type="checkbox"
                    className="mt-0.5 h-4 w-4"
                    checked={acknowledged.has(c.code)}
                    onChange={(e) => onToggleAck(c.code, e.target.checked)}
                    aria-label={`Acknowledge ${c.code}`}
                  />
                  <span>
                    I acknowledge that {c.code} is not passing
                    {c.code === 'R-PLAYER-FUNDS' && mode === 'close' ? `. ${FUNDS_STRANDED_NOTICE}` : '.'}
                  </span>
                </label>
              )}
            </li>
          )
        })}
      </ul>
      <FundsView readiness={readiness} mode={mode} />
    </div>
  )
}
