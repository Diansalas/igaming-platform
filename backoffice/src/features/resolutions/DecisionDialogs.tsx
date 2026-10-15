import { useMutation } from '@tanstack/react-query'
import { useRef, useState, type ReactNode } from 'react'
import { newIdempotencyKey } from '../../api/idempotency'
import { Button } from '../../components/Button'
import { Checkbox, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { ActingOn, ConsoleErrorAlert, Hint } from '../consoles/ConsoleBits'
import { describeRefusalCode, refusalIsStale, type ConsoleFamily } from '../consoles/errors'
import { safeCode } from '../killswitch/labels'

export interface DecisionOutcome {
  resolution: { state: string; refusal_code?: string; ledger_transaction_id?: string; payload_hash: string }
  executed: boolean
  refused: boolean
  counted_approvals: number
  required_approvals: number
}

export function describeOutcome(op: 'approve' | 'reject', o: DecisionOutcome): string {
  if (op === 'reject') return 'Rejected. The resolution is closed and nothing was executed.'
  if (o.executed) {
    const tx = o.resolution.ledger_transaction_id
    return `Executed. The final approval was recorded and the resolution ran in the same step${tx ? ` (ledger transaction ${safeCode(tx)})` : ''}. This is a MOCK-stack action.`
  }
  if (o.refused) return `Refused at execution: ${describeRefusalCode(o.resolution.refusal_code)}. The approval was recorded, the resolution did NOT run and no money moved.`
  return `Approval recorded: ${o.counted_approvals} of ${o.required_approvals} required. The resolution stays pending until the remaining approvals are recorded.`
}

/** The confirmation string for a decision: the LAST 8 characters of the payload hash. It is not printed in any label. */
export const hashConfirmation = (hash: string) => hash.slice(-8)

/**
 * Approve or reject with a frozen payload hash. The dialog is opened on a SNAPSHOT of the resolution; the hash
 * shown and the hash sent are that snapshot's value, whatever a later refresh returns (ADR 0114 principle 3). The
 * summary lists every field the hash covers so the approver reviews all of it. The operator confirms by typing the
 * LAST 8 characters of the hash they are looking at (never printed in the label, so it cannot be copied from the
 * instruction). The server is the authority on distinct-person, grant, proof and recount; the self-approval hint
 * only disables a button.
 */
export function DecisionDialog({
  op,
  title,
  family,
  tenantId,
  tenantLabel,
  summary,
  consequence,
  frozenHash,
  liveHash,
  reasonOk,
  reasonHelp,
  send,
  onClose,
  onDone,
  onRefused,
}: {
  op: 'approve' | 'reject'
  title: string
  family: ConsoleFamily
  tenantId: string
  tenantLabel: string | undefined
  summary: Array<[string, ReactNode]>
  consequence: string
  frozenHash: string
  /** The hash the latest refresh returned for this resolution, when it differs the dialog warns and blocks. */
  liveHash: string | undefined
  reasonOk: (reason: string) => boolean
  reasonHelp: string
  send: (hash: string, reason: string, key: string) => Promise<DecisionOutcome>
  onClose: () => void
  onDone: () => void
  onRefused: () => void
}) {
  const [reason, setReason] = useState('')
  const [typed, setTyped] = useState('')
  const [ack, setAck] = useState(false)
  const [key] = useState(newIdempotencyKey)
  const [sent, setSent] = useState(0)
  const busy = useRef(false)
  const [outcome, setOutcome] = useState<DecisionOutcome | null>(null)
  const m = useMutation({
    mutationFn: () => {
      setSent((n) => n + 1)
      return send(frozenHash, reason, key)
    },
    onSuccess: (o) => {
      setOutcome(o)
      onRefused() // refresh the list behind the dialog: the state changed
    },
    onError: (e) => {
      if (refusalIsStale(e, family, op)) onRefused()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  const changed = liveHash !== undefined && liveHash !== frozenHash
  const need = hashConfirmation(frozenHash)
  const valid = reasonOk(reason) && ack && typed === need && !changed

  if (outcome) {
    return (
      <Modal title={title} onClose={onDone}>
        <div className="flex flex-col gap-4">
          <p role="status" className="text-sm text-slate-800">{describeOutcome(op, outcome)}</p>
          <div className="flex justify-end"><Button onClick={onDone}>Close</Button></div>
        </div>
      </Modal>
    )
  }
  return (
    <Modal title={title} onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <p className="text-sm font-bold text-red-800">MOCK stack - software capability only, not a regulatory approval.</p>
        <ActingOn tenantId={tenantId} label={tenantLabel} />
        <dl className="grid grid-cols-[12rem_1fr] gap-y-1 text-sm">
          {summary.map(([k, v]) => (
            <div key={k} className="contents"><dt className="text-slate-500">{k}</dt><dd>{v}</dd></div>
          ))}
          <dt className="text-slate-500">Payload hash you are deciding on</dt>
          <dd className="break-all font-mono text-xs" data-testid="frozen-hash">{frozenHash}</dd>
        </dl>
        <p className="text-sm text-slate-700">{consequence}</p>
        {changed && <Hint>This resolution changed after you opened this dialog (its hash is different now). Close the dialog and review it again; the server would refuse this decision.</Hint>}
        <TextInput label="Reason code (required)" value={reason} onChange={(e) => setReason(e.target.value)} help={reasonHelp} autoComplete="off" />
        <Checkbox label="I reviewed every field listed above and the hash shown is the one I am deciding on" checked={ack} onChange={(e) => setAck(e.target.checked)} />
        <TextInput label="Type the LAST 8 characters of the payload hash shown above to confirm" value={typed} onChange={(e) => setTyped(e.target.value.trim())} autoComplete="off" spellCheck={false} />
        <ConsoleErrorAlert error={m.error} family={family} op={op} retried={sent > 1} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Back</Button>
          <Button
            variant={op === 'approve' ? 'danger' : 'primary'}
            disabled={!valid}
            isLoading={m.isPending}
            onClick={() => {
              if (busy.current) return
              busy.current = true
              m.mutate()
            }}
          >
            {op === 'approve' ? 'Approve' : 'Reject'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

export function CancelDialog({
  title,
  family,
  tenantId,
  tenantLabel,
  detail,
  send,
  onClose,
  onDone,
  onRefused,
}: {
  title: string
  family: ConsoleFamily
  tenantId: string
  tenantLabel: string | undefined
  detail: string
  send: (key: string) => Promise<unknown>
  onClose: () => void
  onDone: () => void
  onRefused: () => void
}) {
  const [key] = useState(newIdempotencyKey)
  const [sent, setSent] = useState(0)
  const busy = useRef(false)
  const m = useMutation({
    mutationFn: () => {
      setSent((n) => n + 1)
      return send(key)
    },
    onSuccess: onDone,
    onError: (e) => {
      if (refusalIsStale(e, family, 'cancel')) onRefused()
    },
    onSettled: () => {
      busy.current = false
    },
  })
  return (
    <Modal title={title} onClose={onClose}>
      <div className="flex flex-col gap-4">
        <ActingOn tenantId={tenantId} label={tenantLabel} />
        <p className="text-sm text-slate-700">{detail}</p>
        <ConsoleErrorAlert error={m.error} family={family} op="cancel" retried={sent > 1} />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={m.isPending}>Back</Button>
          <Button isLoading={m.isPending} onClick={() => { if (busy.current) return; busy.current = true; m.mutate() }}>Cancel the request</Button>
        </div>
      </div>
    </Modal>
  )
}
