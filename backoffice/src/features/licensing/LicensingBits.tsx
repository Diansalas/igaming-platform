import { Link } from 'react-router-dom'
import { Badge } from '../../components/Badge'
import { describeLicensingError } from './errors'
import {
  APPROVAL_RULES,
  EVIDENCE_NOTE,
  LICENSING_DISCLAIMER,
  REGISTRY_NOTE,
  changeStatus,
  licenceStatus,
  safeToken,
} from './labels'
import type { FilingResult } from '../../api/licensing'
import type { ReactNode } from 'react'

export function LicensingErrorAlert({ error }: { error: unknown }) {
  if (!error) return null
  const v = describeLicensingError(error)
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{v.message}</p>
      {(v.status || v.code) && (
        <p className="mt-1 text-xs text-red-700">
          {v.status ? `HTTP ${v.status}` : ''}
          {v.status && v.code ? ' - ' : ''}
          {v.code ? <span className="font-mono">{safeToken(v.code)}</span> : null}
        </p>
      )}
      {v.lcCodes.length > 0 && (
        <ul className="mt-1 list-disc pl-5 text-xs text-red-700">
          {v.lcCodes.map((c) => (
            <li key={c.code}>
              <span className="font-mono">{c.code}</span>: {c.text}
            </li>
          ))}
        </ul>
      )}
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {safeToken(v.requestId) === 'unprintable' ? 'unprintable' : v.requestId}</p>}
    </div>
  )
}

export function Disclaimer() {
  return (
    <p role="note" aria-label="Disclaimer" className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
      {LICENSING_DISCLAIMER}
    </p>
  )
}

export function RegistryNote() {
  return (
    <p role="note" aria-label="Registry note" className="text-xs text-slate-500">
      {REGISTRY_NOTE}
    </p>
  )
}

export function EvidenceNote() {
  return (
    <p role="note" aria-label="Evidence reference note" className="text-xs text-slate-600">
      {EVIDENCE_NOTE}
    </p>
  )
}

export function ApprovalRules() {
  return (
    <section aria-label="Approval rules" className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
      <p className="font-medium">How approval works</p>
      <ul className="mt-1 list-disc pl-5">
        {APPROVAL_RULES.map((r) => (
          <li key={r}>{r}</li>
        ))}
      </ul>
    </section>
  )
}

export function ChangeStatusBadge({ status }: { status: string }) {
  const s = changeStatus(status)
  return <Badge tone={s.tone}>{s.label}</Badge>
}

export function LicenceStatusBadge({ status }: { status: string }) {
  const s = licenceStatus(status)
  return <Badge tone={s.tone}>{s.label}</Badge>
}

export function Success({ children }: { children: ReactNode }) {
  return (
    <p role="status" className="rounded-md border border-green-200 bg-green-50 px-3 py-2 text-sm text-green-800">
      {children}
    </p>
  )
}

export function Pending({ children }: { children: ReactNode }) {
  return (
    <p role="status" className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900">
      {children}
    </p>
  )
}

/**
 * The outcome of a filing call. A 202 is shown as "pending approval" and NEVER as success; only a 200 that carries the
 * classified change is shown as applied; a body that matches neither is "not confirmed".
 */
export function FilingOutcome({ result }: { result: FilingResult }) {
  switch (result.kind) {
    case 'pending': {
      const c = result.response.change_request
      return (
        <Pending>
          Pending approval. Nothing has changed yet. This is a widening change: it needs {c.required_approvals} approvals from different people before it takes effect.{' '}
          <Link className="font-medium underline" to={`/licensing/changes/${encodeURIComponent(c.id)}`}>
            Open request {safeToken(c.id).slice(0, 8)}
          </Link>
        </Pending>
      )
    }
    case 'applied':
      return (
        <Success>
          Applied at once. This was a {result.change.change_kind} change, so no second approval was needed.
          {result.change.change_request_id ? (
            <>
              {' '}
              <Link className="font-medium underline" to={`/licensing/changes/${encodeURIComponent(result.change.change_request_id)}`}>
                Open record
              </Link>
            </>
          ) : null}
        </Success>
      )
    case 'unbound':
      return <Success>The tenant now has no licence bound (an unbind is a narrowing change and executed at once).</Success>
    default:
      return <Pending>The server answered, but the answer could not be recognised. Nothing is confirmed. Check the licensing changes list before trying again.</Pending>
  }
}

/** Short, printable id prefix. */
export const short = (id: string | null | undefined) => (typeof id === 'string' && /^[0-9a-fA-F-]{8,40}$/.test(id) ? id.slice(0, 8) : 'unprintable')
