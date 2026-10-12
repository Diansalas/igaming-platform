import { useMutation } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { KILL_SWITCH_REASON_MAX, engageKillSwitch, reasonByteLength, reasonCodeValid, type KillSwitch, type KillSwitchOperation } from '../../api/killSwitch'
import { Button } from '../../components/Button'
import { Checkbox, SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { KillSwitchErrorAlert } from './KillSwitchBits'
import { refusalIsStale } from './errors'
import { CONSEQUENCE_ENGAGE, MOCK_BANNER_TITLE, NOTICE_REENGAGE, NOTICE_TAKEOVER_PLATFORM, NOTICE_TAKEOVER_TENANT, REASON_HELP } from './labels'

const OPERATIONS: { value: KillSwitchOperation; label: string }[] = [
  { value: 'payout', label: 'Payouts (outbound withdrawals)' },
  { value: 'deposit', label: 'Deposits' },
  { value: '*', label: 'All operations' },
]

/**
 * Engage is single-actor by design (ADR 0095 section 10.4: an incident control must not wait for a second
 * person), so the guard here is explicit consequence text plus an acknowledgement, not four-eyes. Release is
 * the four-eyes half. The server validates the provider ('*' or a registered id); there is no provider list
 * route, so the provider is typed.
 */
export function EngageDialog({
  tenantId,
  viewerIsPlatform,
  onClose,
  onDone,
  onRefused,
}: {
  tenantId: string
  viewerIsPlatform: boolean
  onClose: () => void
  onDone: (ks: KillSwitch) => void
  onRefused: () => void
}) {
  const [provider, setProvider] = useState('*')
  const [operation, setOperation] = useState<KillSwitchOperation>('payout')
  const [reason, setReason] = useState('')
  const [ack, setAck] = useState(false)
  const busy = useRef(false)
  const m = useMutation({
    mutationFn: () => engageKillSwitch(tenantId, { provider_scope: provider, operation_scope: operation, reason_code: reason }),
    onSuccess: onDone,
    onError: (e) => {
      if (refusalIsStale(e, 'engage')) onRefused()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  const bytes = reasonByteLength(reason)
  const valid = provider !== '' && provider === provider.trim() && reasonCodeValid(reason) && ack
  return (
    <Modal title="Engage payment kill switch" onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm font-bold text-red-800">{MOCK_BANNER_TITLE}</p>
        <ul className="list-disc pl-5 text-sm text-slate-700" aria-label="Consequences">
          {CONSEQUENCE_ENGAGE.map((t) => (
            <li key={t}>{t}</li>
          ))}
        </ul>
        <div aria-label="Re-engage and takeover" role="note" className="flex flex-col gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
          <p>{NOTICE_REENGAGE}</p>
          <p>{viewerIsPlatform ? NOTICE_TAKEOVER_PLATFORM : NOTICE_TAKEOVER_TENANT}</p>
        </div>
        <TextInput
          label="Provider"
          value={provider}
          onChange={(e) => setProvider(e.target.value)}
          help="Enter * for every provider, or one provider id. The server refuses an id that is not registered on this deployment."
          autoComplete="off"
        />
        <SelectField label="Operation" value={operation} onChange={(e) => setOperation(e.target.value as KillSwitchOperation)} options={OPERATIONS} />
        <TextInput
          label="Reason code (required)"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          help={
            <>
              {REASON_HELP}
              {bytes > KILL_SWITCH_REASON_MAX && (
                <span role="alert" className="mt-1 block text-red-700">
                  This reason code is {bytes} bytes; the limit is {KILL_SWITCH_REASON_MAX} bytes. Shorten it.
                </span>
              )}
            </>
          }
          autoComplete="off"
        />
        <Checkbox label="I understand this stops outbound dispatch and does not release funds or cancel withdrawals" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <KillSwitchErrorAlert error={m.error} op="engage" />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>
            Cancel
          </Button>
          <Button
            variant="danger"
            disabled={!valid}
            isLoading={m.isPending}
            onClick={() => {
              if (busy.current) return
              busy.current = true
              m.mutate()
            }}
          >
            Engage kill switch
          </Button>
        </div>
      </div>
    </Modal>
  )
}
