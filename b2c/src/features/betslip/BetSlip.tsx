import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useAuth } from '../../auth/AuthContext'
import { getEvent, placeBet, type Bet, type RejectionCategory } from '../../api/sportsbook'
import { ApiError } from '../../api/types'
import { Button } from '../../components/Button'
import { TextInput } from '../../components/TextInput'
import { brandConfig } from '../../config/brand'
import { newIdempotencyKey } from '../../lib/idempotencyKey'
import { formatMoney, toMinorUnits } from '../../lib/money'
import { estimatePotentialReturnMinorUnits, formatDecimalOdds } from '../../lib/odds'
import { useBetSlip } from './BetSlipContext'

type SubmitOutcome =
  | { kind: 'accepted'; bet: Bet }
  | { kind: 'rejected'; category?: RejectionCategory; message: string }
  | { kind: 'error'; message: string }

const REJECTION_COPY: Record<RejectionCategory, { title: string; hint: string }> = {
  odds_changed: {
    title: 'The odds changed',
    hint: 'The price for this selection moved before your bet was placed. Review the new price below and try again if you still want this bet.',
  },
  event_not_open: {
    title: 'This event is no longer open for betting',
    hint: 'The event or market closed before your bet was placed. Choose a different selection.',
  },
  insufficient_funds: {
    title: 'Not enough funds',
    hint: 'Your available balance does not cover this stake.',
  },
  rg_denied: {
    title: 'This bet cannot be placed',
    hint: 'A responsible-gaming control on your account is preventing this bet.',
  },
  risk_denied: {
    title: 'This bet cannot be placed',
    hint: 'Please contact support if you believe this is a mistake.',
  },
}

/**
 * Persistent, singles-only bet slip. This component makes NO decision
 * about whether a bet succeeds - it always calls POST
 * /v1/me/sportsbook/bets and renders exactly what the server returns,
 * including a real rejection. The stake/estimated-return numbers shown
 * before submission are clearly-labeled, client-side, DISPLAY-only
 * convenience; the confirmation shown after a 201 always uses the
 * server's own `bet.potential_return` and `bet.odds_*` fields, never this
 * component's own pre-calculation.
 */
export function BetSlip() {
  const { selection, updateOdds, clear, isOpen, close } = useBetSlip()
  const { isAuthenticated } = useAuth()
  const [stakeInput, setStakeInput] = useState('')
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [outcome, setOutcome] = useState<SubmitOutcome | null>(null)
  const [isRefreshingOdds, setIsRefreshingOdds] = useState(false)

  if (!selection) return null

  const assetCode = brandConfig.defaultAssetCode
  const stakeMinorUnits = toMinorUnits(stakeInput, assetCode)
  const decimalOdds = formatDecimalOdds(selection.oddsNumerator, selection.oddsDenominator)
  const estimatedReturn =
    stakeMinorUnits !== null ? estimatePotentialReturnMinorUnits(stakeMinorUnits, selection.oddsNumerator, selection.oddsDenominator) : null

  async function refetchOdds() {
    if (!selection) return
    setIsRefreshingOdds(true)
    try {
      const event = await getEvent(selection.eventId)
      for (const market of event.markets) {
        const match = market.selections.find((s) => s.id === selection.selectionId)
        if (match) {
          updateOdds(match.odds_numerator, match.odds_denominator)
          break
        }
      }
    } catch {
      // Best-effort refresh; leave the old (known-stale) odds displayed if
      // this fails - the next real submit attempt will surface any real
      // problem from the server directly.
    } finally {
      setIsRefreshingOdds(false)
    }
  }

  async function onPlaceBet() {
    if (!selection || stakeMinorUnits === null) return
    setIsSubmitting(true)
    setOutcome(null)
    try {
      const result = await placeBet({
        selectionId: selection.selectionId,
        stakeAmount: stakeMinorUnits,
        assetCode,
        expectedOddsNumerator: selection.oddsNumerator,
        expectedOddsDenominator: selection.oddsDenominator,
        idempotencyKey: newIdempotencyKey(),
      })
      if (result.accepted && result.bet) {
        setOutcome({ kind: 'accepted', bet: result.bet })
      } else {
        const category = result.rejection_category
        const message = result.rejection_message || 'This bet was not accepted.'
        setOutcome({ kind: 'rejected', category, message })
        if (category === 'odds_changed') {
          void refetchOdds()
        }
      }
    } catch (err) {
      const message = err instanceof ApiError ? err.message : 'Something went wrong placing this bet. Please try again.'
      setOutcome({ kind: 'error', message })
    } finally {
      setIsSubmitting(false)
    }
  }

  function onStartOver() {
    clear()
    setStakeInput('')
    setOutcome(null)
  }

  const panel = (
    <div className="flex h-full flex-col">
      <div className="flex items-center justify-between border-b border-border px-4 py-3">
        <h2 className="text-sm font-semibold text-slate-900">Bet slip</h2>
        <button
          type="button"
          onClick={close}
          aria-label="Close bet slip"
          className="rounded p-1 text-slate-400 hover:bg-surface-alt hover:text-slate-600 lg:hidden"
        >
          &#10005;
        </button>
      </div>

      <div className="flex-1 overflow-y-auto p-4">
        {outcome?.kind === 'accepted' ? (
          <div className="flex flex-col gap-3" role="status">
            <p className="text-sm font-medium text-green-700">Bet placed.</p>
            <dl className="grid grid-cols-2 gap-2 text-sm">
              <dt className="text-slate-500">Bet ID</dt>
              <dd className="break-all font-mono text-xs text-slate-700">{outcome.bet.id}</dd>
              <dt className="text-slate-500">Stake</dt>
              <dd>{formatMoney(outcome.bet.stake_amount, outcome.bet.asset_code)}</dd>
              <dt className="text-slate-500">Odds</dt>
              <dd>{formatDecimalOdds(outcome.bet.odds_numerator, outcome.bet.odds_denominator)}</dd>
              <dt className="text-slate-500">Potential return</dt>
              <dd className="font-medium text-slate-900">{formatMoney(outcome.bet.potential_return, outcome.bet.asset_code)}</dd>
              <dt className="text-slate-500">Status</dt>
              <dd className="capitalize">{outcome.bet.status}</dd>
            </dl>
            <p className="text-xs text-slate-500">
              The stake, odds and potential return above are exactly what the server recorded for this bet.
            </p>
            <div className="flex gap-2">
              <Link to="/account/bets" className="text-sm text-brand-700 hover:underline">
                View bet history
              </Link>
            </div>
            <Button variant="secondary" onClick={onStartOver}>
              Place another bet
            </Button>
          </div>
        ) : (
          <div className="flex flex-col gap-4">
            <div className="rounded-md border border-border p-3">
              <p className="text-xs text-slate-500">{selection.eventName}</p>
              <p className="text-sm font-medium text-slate-900">{selection.selectionName}</p>
              <p className="text-xs text-slate-500">{selection.marketName}</p>
              <p className="mt-2 text-sm font-semibold text-brand-700">{decimalOdds}</p>
            </div>

            {outcome && (outcome.kind === 'rejected' || outcome.kind === 'error') && (
              <div role="alert" className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800">
                {outcome.kind === 'rejected' ? (
                  <>
                    <p className="font-medium">{outcome.category ? REJECTION_COPY[outcome.category].title : 'Bet not accepted'}</p>
                    <p className="mt-1 text-red-700">{outcome.category ? REJECTION_COPY[outcome.category].hint : outcome.message}</p>
                    {outcome.category === 'odds_changed' && (
                      <p className="mt-2 text-xs text-red-700">
                        {isRefreshingOdds ? 'Refreshing price...' : `New price: ${decimalOdds}. Check it and try again if you still want this bet.`}
                      </p>
                    )}
                    {outcome.category === 'insufficient_funds' && (
                      <Link to="/account/deposit" className="mt-2 inline-block text-sm font-medium text-brand-700 hover:underline">
                        Deposit funds
                      </Link>
                    )}
                  </>
                ) : (
                  <p>{outcome.message}</p>
                )}
              </div>
            )}

            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium text-slate-700">Stake ({assetCode})</span>
              <TextInput
                type="number"
                min="0"
                step="0.01"
                inputMode="decimal"
                value={stakeInput}
                onChange={(e) => setStakeInput(e.target.value)}
                placeholder="0.00"
              />
            </label>

            <div className="rounded-md bg-surface-alt p-3 text-sm">
              <div className="flex justify-between text-slate-500">
                <span>Estimated return</span>
                <span>{estimatedReturn !== null ? formatMoney(estimatedReturn, assetCode) : '-'}</span>
              </div>
              <p className="mt-1 text-xs text-slate-400">Estimate only, before the bet is placed - not the server's final figure.</p>
            </div>

            {!isAuthenticated ? (
              <div className="flex flex-col gap-2">
                <p className="text-sm text-slate-600">Log in to place this bet.</p>
                <Link to="/login">
                  <Button className="w-full">Log in</Button>
                </Link>
              </div>
            ) : (
              <Button className="w-full" isLoading={isSubmitting} disabled={stakeMinorUnits === null} onClick={() => void onPlaceBet()}>
                Place bet
              </Button>
            )}

            <button type="button" onClick={onStartOver} className="text-xs text-slate-400 hover:underline">
              Remove selection
            </button>
          </div>
        )}
      </div>
    </div>
  )

  return (
    <>
      {/* Desktop: persistent right-hand sidebar. */}
      <aside className="hidden w-80 shrink-0 border-l border-border bg-surface lg:block">{panel}</aside>

      {/* Mobile: bottom sheet, toggled by the floating button in AppLayout. */}
      {isOpen && (
        <div className="fixed inset-0 z-40 flex flex-col justify-end bg-slate-900/40 lg:hidden" onClick={close}>
          <div className="max-h-[80vh] rounded-t-xl bg-surface shadow-xl" onClick={(e) => e.stopPropagation()}>
            {panel}
          </div>
        </div>
      )}
    </>
  )
}
