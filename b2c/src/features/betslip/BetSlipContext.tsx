import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'
import { newIdempotencyKey } from '../../lib/idempotencyKey'

// Singles-only bet slip state (matching the backend's scope this stage -
// no accumulator/multi-leg support). Selecting a new outcome REPLACES
// whatever was in the slip, exactly like a real sportsbook's single-slip
// UX. This holds only display data copied at selection time (name, event,
// odds) - it is never treated as authoritative. The actual bet placement
// call always sends `expected_odds_numerator`/`expected_odds_denominator`
// so the server can detect and reject a stale price
// (`rejection_category: odds_changed`) rather than trusting this slip's
// copy of the odds.
//
// idempotencyKey is minted ONCE per selection (here, in setSelection) and
// held for the lifetime of this slip composition - Stage 6.1 security
// review finding: the original design minted a FRESH key on every submit
// attempt (inside onPlaceBet itself), which defeats the server's
// idempotency guarantee for the exact case it exists to cover - a
// network/timeout error where the original POST actually committed but
// the response was lost. Retrying with a NEW key placed a genuine second
// bet and a second stake debit. The key now survives a retry after any
// error, and is regenerated only when the player picks a different
// selection (a new SlipSelection) or explicitly starts over (clear()) -
// both cases are genuinely a different bet attempt, not a retry of the
// same one.
export interface SlipSelection {
  selectionId: string
  selectionName: string
  marketName: string
  eventId: string
  eventName: string
  oddsNumerator: number
  oddsDenominator: number
  idempotencyKey: string
}

/** What a caller (e.g. EventDetailPage) provides when picking a selection - everything except the idempotency key, which this context mints itself. */
export type NewSlipSelection = Omit<SlipSelection, 'idempotencyKey'>

interface BetSlipContextValue {
  selection: SlipSelection | null
  setSelection: (selection: NewSlipSelection) => void
  /** Updates only the odds of the CURRENT selection - used after an odds_changed rejection re-fetches the live price. Deliberately does NOT touch idempotencyKey: this is still the same bet attempt, now armed with a fresher price. */
  updateOdds: (oddsNumerator: number, oddsDenominator: number) => void
  clear: () => void
  isOpen: boolean
  open: () => void
  close: () => void
  toggle: () => void
}

const BetSlipContext = createContext<BetSlipContextValue | null>(null)

export function BetSlipProvider({ children }: { children: ReactNode }) {
  const [selection, setSelectionState] = useState<SlipSelection | null>(null)
  const [isOpen, setIsOpen] = useState(false)

  const setSelection = useCallback((next: NewSlipSelection) => {
    setSelectionState({ ...next, idempotencyKey: newIdempotencyKey() })
    setIsOpen(true)
  }, [])

  const updateOdds = useCallback((oddsNumerator: number, oddsDenominator: number) => {
    setSelectionState((prev) => (prev ? { ...prev, oddsNumerator, oddsDenominator } : prev))
  }, [])

  const clear = useCallback(() => setSelectionState(null), [])
  const open = useCallback(() => setIsOpen(true), [])
  const close = useCallback(() => setIsOpen(false), [])
  const toggle = useCallback(() => setIsOpen((v) => !v), [])

  const value = useMemo<BetSlipContextValue>(
    () => ({ selection, setSelection, updateOdds, clear, isOpen, open, close, toggle }),
    [selection, setSelection, updateOdds, isOpen, open, close, toggle],
  )

  return <BetSlipContext.Provider value={value}>{children}</BetSlipContext.Provider>
}

export function useBetSlip(): BetSlipContextValue {
  const ctx = useContext(BetSlipContext)
  if (!ctx) throw new Error('useBetSlip must be used within a BetSlipProvider')
  return ctx
}
