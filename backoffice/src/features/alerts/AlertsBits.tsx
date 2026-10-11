import { useMutation } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { ackAlert, resolveAlert, ALERT_REASON_CODE_PATTERN, type AlertTransitionResult, type PlatformAlert } from '../../api/alerts'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { describeAlertError, type AlertOp } from './errors'
import { DELIVERY_BANNER_BODY, DELIVERY_BANNER_TITLE, REASON_HELP, describeKind } from './labels'

/**
 * Fixed, always-present banner: ALERT-DELIVERY-1 is OPEN. Not dismissible.
 * HARDCODED on purpose. When ALERT-DELIVERY-1 closes this MUST be derived from GET /v1/admin/alerting/status
 * (human_notification_routes > 0 per severity), not left static; likewise the "Nobody was notified" wording
 * must follow each row's notified_a_person. Deriving it is NOT authorised yet (pinned by a test).
 */
export function DeliveryBanner() {
  return (
    <div role="note" aria-label="Delivery status" className="rounded-md border border-red-300 bg-red-50 px-4 py-3 text-sm text-red-900">
      <p className="font-bold">{DELIVERY_BANNER_TITLE}</p>
      <p className="mt-1">{DELIVERY_BANNER_BODY}</p>
    </div>
  )
}

/** Error banner built from the closed code map only; the server message is never rendered. */
export function AlertErrorAlert({ error, op }: { error: unknown; op: AlertOp }) {
  if (!error) return null
  const v = describeAlertError(error, op)
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

export function KindText({ kind }: { kind: string }) {
  const k = describeKind(kind)
  return (
    <span>
      {k.title}{' '}
      {!k.known && <span className="font-mono text-xs text-slate-500">({k.code})</span>}
    </span>
  )
}

export function TransitionDialog({
  op,
  alert,
  onClose,
  onDone,
  onRefused,
}: {
  op: 'ack' | 'resolve'
  alert: PlatformAlert
  onClose: () => void
  onDone: (r: AlertTransitionResult) => void
  /** Called after any refusal so the caller can re-read the list (the state may have moved). */
  onRefused: () => void
}) {
  const [reason, setReason] = useState('')
  const busy = useRef(false)
  const required = op === 'resolve'
  const m = useMutation({
    mutationFn: () => (op === 'ack' ? ackAlert(alert.id, reason || undefined) : resolveAlert(alert.id, reason)),
    onSuccess: onDone,
    onError: () => onRefused(),
    onSettled: () => {
      busy.current = false
    },
  })
  const valid = reason === '' ? !required : ALERT_REASON_CODE_PATTERN.test(reason)
  const k = describeKind(alert.kind)
  return (
    <Modal title={op === 'ack' ? 'Acknowledge alert' : 'Resolve alert'} onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-slate-600">
          {op === 'ack'
            ? 'Acknowledging records that you have seen this alert. It does not fix anything and does not notify anyone.'
            : 'Resolving closes this alert. It does not fix the underlying cause and does not notify anyone.'}{' '}
          Alert: <strong>{k.title}</strong>
          {!k.known && <span className="font-mono text-xs"> ({k.code})</span>}.
        </p>
        <TextInput
          label={required ? 'Reason code (required)' : 'Reason code (optional)'}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          help={REASON_HELP}
          maxLength={64}
          autoComplete="off"
          aria-invalid={reason !== '' && !valid}
        />
        <AlertErrorAlert error={m.error} op={op} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>
            Cancel
          </Button>
          <Button
            onClick={() => {
              if (busy.current) return
              busy.current = true
              m.mutate()
            }}
            disabled={!valid}
            isLoading={m.isPending}
          >
            {op === 'ack' ? 'Acknowledge' : 'Resolve'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
