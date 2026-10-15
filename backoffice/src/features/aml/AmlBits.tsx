import { useQuery } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { AML_REASON_CODE } from '../../api/aml'
import { listTenants } from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { isPlatformAdmin } from '../../auth/jwt'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { Select } from '../../components/Select'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { describeAmlError, type AmlOp } from './errors'

/** The refusal banner. Fixed sentences from the closed map; the server message is never rendered. */
export function AmlErrorAlert({ error, op }: { error: unknown; op: AmlOp }) {
  if (!error) return null
  const v = describeAmlError(error, op)
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{v.message}</p>
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {v.requestId}</p>}
    </div>
  )
}

export function ConfidentialityBanner() {
  return (
    <p role="note" className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-900">
      AML records are confidential. Do not discuss, forward or reveal a hold, a case or a screening result to the player or to staff without AML access
      (tipping-off). This screen is a software capability, not a regulatory approval; transaction monitoring and SAR export are not implemented.
    </p>
  )
}

/**
 * The tenant this view acts in. A tenant-scoped officer always acts in its own tenant (claims); a platform officer picks a tenant
 * (the server then requires an in-force acting grant for exactly that tenant). The pick is UI state only: the path value is what
 * the server validates.
 */
export function useAmlTenant() {
  const { claims } = useAuth()
  const platform = isPlatformAdmin(claims)
  const [picked, setPicked] = useState('')
  const [search, setSearch] = useState('')
  const debounced = useDebouncedValue(search, 250)
  const tenants = useQuery({
    queryKey: ['aml', 'tenants', debounced],
    queryFn: () => listTenants({ q: debounced || undefined, limit: 50 }),
    enabled: platform,
    retry: false,
  })
  const tenantId = platform ? picked : (claims?.tenant_id ?? '')
  const picker: ReactNode = platform ? (
    <div className="flex flex-wrap items-end gap-2">
      <TextInput label="Find tenant" value={search} onChange={(e) => setSearch(e.target.value)} autoComplete="off" />
      <Select
        aria-label="Tenant"
        placeholder="Choose a tenant"
        options={(tenants.data?.items ?? []).map((t) => ({ value: t.id, label: t.name }))}
        value={picked}
        onChange={(e) => setPicked(e.target.value)}
      />
      {tenants.error != null && <p className="text-sm text-red-700">The tenant list could not be loaded.</p>}
    </div>
  ) : (
    <p className="text-sm text-slate-600">Acting in your own tenant.</p>
  )
  return { tenantId, picker, platform }
}

/** A dialog that collects ONE reason code (the closed server token) and confirms. */
export function ReasonDialog({
  title,
  description,
  confirmLabel,
  isSubmitting,
  error,
  op,
  extra,
  extraValid = true,
  onConfirm,
  onCancel,
}: {
  title: string
  description?: ReactNode
  confirmLabel: string
  isSubmitting: boolean
  error: unknown
  op: AmlOp
  extra?: ReactNode
  extraValid?: boolean
  onConfirm: (reason: string) => void
  onCancel: () => void
}) {
  const [reason, setReason] = useState('')
  const valid = AML_REASON_CODE.test(reason) && extraValid
  return (
    <Modal title={title} onClose={onCancel}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (valid && !isSubmitting) onConfirm(reason)
        }}
      >
        {description && <div className="text-sm text-slate-600">{description}</div>}
        {extra}
        <TextInput
          label="Reason code"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          autoComplete="off"
          autoFocus
          help="Lower-case letters, digits and underscores, up to 64. A short code, never free text or personal data."
          aria-invalid={reason !== '' && !AML_REASON_CODE.test(reason)}
        />
        <AmlErrorAlert error={error} op={op} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onCancel} disabled={isSubmitting}>
            Cancel
          </Button>
          <Button type="submit" disabled={!valid} isLoading={isSubmitting}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/** A shortened id; a redacted (null) id is shown as a fixed word, never guessed. */
export function shortId(id: string | null | undefined): string {
  if (!id) return 'hidden'
  return id.length > 8 ? `${id.slice(0, 8)}…` : id
}
