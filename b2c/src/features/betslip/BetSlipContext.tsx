import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'

// Singles-only bet slip state (matching the backend's scope this stage -
// no accumulator/multi-leg support). Selecting a new outcome REPLACES
// whatever was in the slip, exactly like a real sportsbook's single-slip
// UX. This holds only display data copied at selection time (name, event,
// odds) - it is never treated as authoritative. The actual bet placement
// call always sends `expected_odds_numerator`/`expected_odds_denominator`
// so the server can detect and reject a stale price
// (`rejection_category: odds_changed`) rather than trusting this slip's
// copy of the odds.
export interface SlipSelection {
  selectionId: string
  selectionName: string
  marketName: string
  eventId: string
  eventName: string
  oddsNumerator: number
  oddsDenominator: number
}

interface BetSlipContextValue {
  selection: SlipSelection | null
  setSelection: (selection: SlipSelection) => void
  /** Updates only the odds of the CURRENT selection - used after an odds_changed rejection re-fetches the live price. */
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

  const setSelection = useCallback((next: SlipSelection) => {
    setSelectionState(next)
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
