import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useLocation, useParams } from 'react-router-dom'
import {
  placeCasinoWager,
  rollbackCasinoPlay,
  settleCasinoWin,
  type CasinoGame,
  type CasinoLaunchResponse,
  type CasinoPlayResult,
} from '../../api/casino'
import { ApiError } from '../../api/types'
import { getMyWallet } from '../../api/wallet'
import { Badge, type BadgeTone } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { TextInput } from '../../components/TextInput'
import { brandConfig } from '../../config/brand'
import { formatMoney, toMinorUnits } from '../../lib/money'

interface SessionLocationState {
  launch?: CasinoLaunchResponse
  game?: Pick<CasinoGame, 'id' | 'name'>
}

const OUTCOME_TONE: Record<CasinoPlayResult['outcome'], BadgeTone> = {
  succeeded: 'success',
  declined: 'danger',
  ambiguous: 'warning',
  pending: 'info',
}

/** Renders one wager/win/rollback outcome exactly as the server returned it - a decline is a normal result, not an error. */
function PlayResultPanel({ title, result }: { title: string; result: CasinoPlayResult }) {
  return (
    <div role="status" className="flex flex-col gap-2 rounded-md border border-border bg-surface-alt p-3 text-sm">
      <div className="flex items-center justify-between">
        <span className="font-medium text-slate-900">{title}</span>
        <Badge tone={OUTCOME_TONE[result.outcome]}>{result.outcome}</Badge>
      </div>
      {result.outcome === 'declined' && (
        <p className="text-red-700">{result.decline_reason || 'This action was declined.'}</p>
      )}
      <dl className="grid grid-cols-1 gap-x-4 gap-y-1 text-xs text-slate-500 sm:grid-cols-2">
        <div className="flex justify-between gap-2 sm:block">
          <dt>Provider tx</dt>
          <dd className="break-all font-mono text-slate-700">{result.provider_tx_id}</dd>
        </div>
        {result.ledger_transaction_id && (
          <div className="flex justify-between gap-2 sm:block">
            <dt>Ledger tx</dt>
            <dd className="break-all font-mono text-slate-700">{result.ledger_transaction_id}</dd>
          </div>
        )}
        {result.tombstoned && <div className="text-amber-700 sm:col-span-2">This transaction was recorded as a tombstone.</div>}
      </dl>
    </div>
  )
}

/**
 * The "game/session screen": there is no real embeddable casino game
 * client in this MVP slice, so this screen itself drives the mock
 * financial simulation via wager/win/rollback. It makes no
 * eligibility/business decision of its own - every button calls an
 * endpoint and renders exactly what comes back, including a decline. The
 * wallet balance is refetched after every action so it never drifts from
 * the server's own view.
 */
export function CasinoSessionPage() {
  const { sessionId: routeSessionId } = useParams<{ sessionId: string }>()
  const location = useLocation()
  const state = (location.state ?? {}) as SessionLocationState
  const sessionId = routeSessionId ?? state.launch?.session_id ?? ''
  const assetCode = brandConfig.defaultAssetCode
  const queryClient = useQueryClient()

  const walletQuery = useQuery({ queryKey: ['casino', 'wallet', assetCode], queryFn: () => getMyWallet(assetCode) })

  const [wagerInput, setWagerInput] = useState('')
  const [winInput, setWinInput] = useState('')
  const [rollbackTxId, setRollbackTxId] = useState('')

  const [wagerResult, setWagerResult] = useState<CasinoPlayResult | null>(null)
  const [winResult, setWinResult] = useState<CasinoPlayResult | null>(null)
  const [rollbackResult, setRollbackResult] = useState<CasinoPlayResult | null>(null)

  const [isWagering, setIsWagering] = useState(false)
  const [isSettling, setIsSettling] = useState(false)
  const [isRollingBack, setIsRollingBack] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)

  const wagerAmount = toMinorUnits(wagerInput, assetCode)
  const winAmount = toMinorUnits(winInput, assetCode)

  function refreshWallet() {
    void queryClient.invalidateQueries({ queryKey: ['casino', 'wallet', assetCode] })
  }

  async function onWager() {
    if (wagerAmount === null) return
    setIsWagering(true)
    setActionError(null)
    try {
      const result = await placeCasinoWager(sessionId, wagerAmount)
      setWagerResult(result)
      if (result.outcome === 'succeeded') setRollbackTxId(result.provider_tx_id)
      refreshWallet()
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : 'Could not place this wager. Please try again.')
    } finally {
      setIsWagering(false)
    }
  }

  async function onSettleWin() {
    if (winAmount === null) return
    setIsSettling(true)
    setActionError(null)
    try {
      const result = await settleCasinoWin(sessionId, winAmount)
      setWinResult(result)
      if (result.outcome === 'succeeded') setRollbackTxId(result.provider_tx_id)
      refreshWallet()
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : 'Could not settle this win. Please try again.')
    } finally {
      setIsSettling(false)
    }
  }

  async function onRollback() {
    if (!rollbackTxId) return
    setIsRollingBack(true)
    setActionError(null)
    try {
      const result = await rollbackCasinoPlay(sessionId, rollbackTxId)
      setRollbackResult(result)
      refreshWallet()
    } catch (err) {
      setActionError(err instanceof ApiError ? err.message : 'Could not roll back this transaction. Please try again.')
    } finally {
      setIsRollingBack(false)
    }
  }

  if (!sessionId) {
    return <ErrorState error={new Error('No casino session id was provided.')} />
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={state.game?.name ?? 'Casino session'} description={`Session ${sessionId}`} />

      <Card title="Balance">
        {walletQuery.isLoading && <LoadingSpinner label="Loading balance..." />}
        {walletQuery.error && <ErrorState error={walletQuery.error} onRetry={() => void walletQuery.refetch()} />}
        {walletQuery.data && (
          <p className="text-lg font-semibold text-slate-900">{formatMoney(walletQuery.data.available_balance, walletQuery.data.asset_code)}</p>
        )}
      </Card>

      {actionError && <ErrorState error={new Error(actionError)} />}

      <Card title="Place wager">
        <div className="flex flex-col gap-3 sm:max-w-sm">
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Stake ({assetCode})</span>
            <TextInput
              type="number"
              min="0"
              step="0.01"
              inputMode="decimal"
              value={wagerInput}
              onChange={(e) => setWagerInput(e.target.value)}
              placeholder="0.00"
            />
          </label>
          <Button isLoading={isWagering} disabled={wagerAmount === null} onClick={() => void onWager()}>
            Place wager
          </Button>
          {wagerResult && <PlayResultPanel title="Wager result" result={wagerResult} />}
        </div>
      </Card>

      <Card title="Settle win">
        <div className="flex flex-col gap-3 sm:max-w-sm">
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Win amount ({assetCode})</span>
            <TextInput
              type="number"
              min="0"
              step="0.01"
              inputMode="decimal"
              value={winInput}
              onChange={(e) => setWinInput(e.target.value)}
              placeholder="0.00"
            />
          </label>
          <Button variant="secondary" isLoading={isSettling} disabled={winAmount === null} onClick={() => void onSettleWin()}>
            Settle win
          </Button>
          {winResult && <PlayResultPanel title="Win result" result={winResult} />}
        </div>
      </Card>

      <Card title="Roll back a transaction">
        <div className="flex flex-col gap-3 sm:max-w-sm">
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Original provider transaction ID</span>
            <TextInput value={rollbackTxId} onChange={(e) => setRollbackTxId(e.target.value)} placeholder="provider-tx-id" />
          </label>
          <Button variant="danger" isLoading={isRollingBack} disabled={!rollbackTxId} onClick={() => void onRollback()}>
            Roll back
          </Button>
          {rollbackResult && <PlayResultPanel title="Rollback result" result={rollbackResult} />}
        </div>
      </Card>

      <Link to="/casino/history" className="text-sm text-brand-700 hover:underline">
        View casino history &rarr;
      </Link>
    </div>
  )
}
