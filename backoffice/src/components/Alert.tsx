import type { ReactNode } from 'react'
import type { ErrorDetails } from '../lib/apiErrorMessage'

/** Success banner for a completed mutation. */
export function SuccessAlert({ children }: { children: ReactNode }) {
  return (
    <div role="status" className="rounded-md border border-green-200 bg-green-50 px-4 py-3 text-sm text-green-800">
      {children}
    </div>
  )
}

/** Actionable error banner: the API's own message plus its request id. */
export function ErrorAlert({ error }: { error: ErrorDetails | null }) {
  if (!error) return null
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{error.message}</p>
      {error.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {error.requestId}</p>}
    </div>
  )
}

/** Neutral informational note (e.g. "this configuration cannot be read back"). */
export function InfoNote({ children }: { children: ReactNode }) {
  return <div className="rounded-md border border-border bg-surface-alt px-4 py-3 text-sm text-slate-600">{children}</div>
}
