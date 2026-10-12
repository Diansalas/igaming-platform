import { useMutation } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { KILL_SWITCH_REASON_MAX, engageKillSwitch, type KillSwitch, type KillSwitchOperation } from '../../api/killSwitch'
import { Button } from '../../components/Button'
import { Checkbox, SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { KillSwitchErrorAlert } from './KillSwitchBits'
import { CONSEQUENCE_ENGAGE, MOCK_BANNER_TITLE, REASON_HELP } from './labels'

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
  onClose,
  onDone,
  onRefused,
}: {
  tenantId: string
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
    onError: () => onRefused(),
    onSettled: () => {
      busy.current = false
    },
  })
  const valid = provider !== '' && provider === provider.trim() && reason.trim() !== '' && reason.length <= KILL_SWITCH_REASON_MAX && ack
  return (
    <Modal title="Engage payment kill switch" onClose={onClose}>
      <div className="flex flex-col gap-4">
        <p className="text-sm font-bold text-red-800">{MOCK_BANNER_TITLE}</p>
        <ul className="list-disc pl-5 text-sm text-slate-700" aria-label="Consequences">
          {CONSEQUENCE_ENGAGE.map((t) => (
            <li key={t}>{t}</li>
          ))}
        </ul>
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
          help={REASON_HELP}
          maxLength={KILL_SWITCH_REASON_MAX}
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
