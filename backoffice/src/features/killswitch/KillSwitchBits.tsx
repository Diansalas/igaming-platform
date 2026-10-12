import { describeKillSwitchError, type KillSwitchOp } from './errors'
import { MOCK_BANNER_BODY, MOCK_BANNER_TITLE, describeActor, describeOperation, describeProvider, describeScope, type EnumView } from './labels'

/** Always present, not dismissible. See labels.ts for why it is hardcoded. */
export function MockBanner() {
  return (
    <div role="note" aria-label="Stack status" className="rounded-md border border-red-300 bg-red-50 px-4 py-3 text-sm text-red-900">
      <p className="font-bold">{MOCK_BANNER_TITLE}</p>
      <p className="mt-1">{MOCK_BANNER_BODY}</p>
    </div>
  )
}

/** Error banner built from the closed code map only; the server message is never rendered. */
export function KillSwitchErrorAlert({ error, op }: { error: unknown; op: KillSwitchOp }) {
  if (!error) return null
  const v = describeKillSwitchError(error, op)
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{v.message}</p>
      {(v.status || v.code) && (
        <p className="mt-1 text-xs text-red-700">
          {v.status ? `HTTP ${v.status}` : ''}
          {v.status && v.code ? ' - ' : ''}
          {v.code ? <span className="font-mono">{v.code}</span> : null}
        </p>
      )}
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {v.requestId}</p>}
    </div>
  )
}

export function EnumText({ view }: { view: EnumView }) {
  return (
    <span>
      {view.label}
      {!view.known && view.code && <span className="font-mono text-xs text-slate-500"> ({view.code})</span>}
    </span>
  )
}

export function OperationText({ value }: { value: unknown }) {
  return <EnumText view={describeOperation(value)} />
}

export function ScopeText({ value }: { value: unknown }) {
  return <EnumText view={describeScope(value)} />
}

export function ProviderText({ value }: { value: unknown }) {
  const p = describeProvider(value)
  return (
    <span>
      {p.label}
      {p.code && <span className="font-mono text-xs"> {p.code}</span>}
    </span>
  )
}

export function ActorText({ id, scope }: { id: string | null | undefined; scope: string | null | undefined }) {
  return <span>{describeActor(id, scope)}</span>
}
