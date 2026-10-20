import { useState, type ReactNode } from 'react'
import { isReasonCode } from '../../api/licensing'
import { TextInput } from '../../components/FormField'
import { describePolicyError, type PolicyCtx } from './errors'
import { REASON_CODE_HELP, safeToken } from './labels'

export function PolicyErrorAlert({ error, ctx }: { error: unknown; ctx: PolicyCtx }) {
  if (!error) return null
  const v = describePolicyError(error, ctx)
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
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {safeToken(v.requestId) === 'unprintable' ? 'unprintable' : v.requestId}</p>}
    </div>
  )
}

export function Note({ label, children, tone = 'amber' }: { label: string; children: ReactNode; tone?: 'amber' | 'slate' }) {
  const cls = tone === 'amber' ? 'border-amber-200 bg-amber-50 text-amber-900' : 'border-border bg-surface-alt text-slate-700'
  return (
    <p role="note" aria-label={label} className={`rounded-md border px-3 py-2 text-sm ${cls}`}>
      {children}
    </p>
  )
}

/** Reason-code input with the OpenAPI pattern check; nothing is prefilled. */
export function ReasonCodeField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [touched, setTouched] = useState(false)
  return (
    <>
      <TextInput
        label="Reason code"
        value={value}
        onChange={(e) => {
          setTouched(true)
          onChange(e.target.value.trim())
        }}
        autoComplete="off"
        maxLength={64}
        help={REASON_CODE_HELP}
      />
      {touched && value !== '' && !isReasonCode(value) && <p role="alert" className="text-sm text-red-700">That is not a valid reason code.</p>}
    </>
  )
}

export const OUTCOME_UNCONFIRMED = 'The server answered, but the answer could not be recognised. Nothing is confirmed. Check the current state before trying again.'
