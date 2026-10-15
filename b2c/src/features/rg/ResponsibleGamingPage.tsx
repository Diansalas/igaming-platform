import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { getMyRgOptions } from '../../api/rg'
import { listMyWallets } from '../../api/wallet'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { brandConfig } from '../../config/brand'
import { LimitsCard } from './LimitsCard'
import { RestrictionsCard } from './RestrictionsCard'
import { SelfExclusionCard } from './SelfExclusionCard'
import { TimeOutCard } from './TimeOutCard'

/**
 * The player's responsible-gaming screen: active restrictions, deposit / loss / wager limits, time-out and
 * self-exclusion. It uses only the existing /v1/me/rg/* routes. It makes no decision of its own: the
 * choices (periods, durations, ceilings, cooling-off) come from GET /v1/me/rg/options, every effective
 * time and value is the server's, and whether anything blocks play is decided server-side on every deposit,
 * bet and launch.
 */
export function ResponsibleGamingPage() {
  const walletsQuery = useQuery({ queryKey: ['wallets'], queryFn: listMyWallets })
  const [asset, setAsset] = useState(brandConfig.defaultAssetCode)
  const optionsQuery = useQuery({ queryKey: ['rg-options', asset], queryFn: () => getMyRgOptions(asset) })

  const assets = Array.from(new Set([brandConfig.defaultAssetCode, ...(walletsQuery.data ?? []).map((w) => w.asset_code)]))

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Responsible gaming"
        description="Set limits, take a break, or exclude yourself. These tools are here to keep play within your control."
        actions={
          <Link to="/account" className="text-sm text-brand-700 hover:underline">
            Back to my account
          </Link>
        }
      />

      <RestrictionsCard />

      {assets.length > 1 && (
        <Card>
          <label className="flex items-center gap-3 text-sm">
            <span className="text-slate-600">Currency</span>
            <Select aria-label="Currency" value={asset} onChange={(e) => setAsset(e.target.value)} options={assets.map((a) => ({ value: a, label: a }))} />
          </label>
        </Card>
      )}

      {optionsQuery.isLoading && <LoadingSpinner label="Loading your options..." />}
      {optionsQuery.error && <ErrorState error={optionsQuery.error} onRetry={() => void optionsQuery.refetch()} />}
      {optionsQuery.data && (
        <>
          <LimitsCard key={asset} assetCode={asset} options={optionsQuery.data} />
          <TimeOutCard options={optionsQuery.data} />
          <SelfExclusionCard options={optionsQuery.data} />
        </>
      )}

      <p className="text-xs text-slate-500">
        If gambling is causing you harm, please talk to someone you trust or contact a support organisation in your country.
      </p>
    </div>
  )
}
