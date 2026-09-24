import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { getMe } from '../../api/auth'
import { listMyWallets } from '../../api/wallet'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { EmptyState } from '../../components/EmptyState'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { useAuth } from '../../auth/AuthContext'
import { formatMoney } from '../../lib/money'
import { ComplianceStatusCard } from './ComplianceStatusCard'
import { EmailVerificationCard } from './EmailVerificationCard'

/**
 * Renders exactly what GET /v1/me and GET /v1/me/wallets return - no
 * balance is ever computed or adjusted client-side. Every field shown
 * here (kyc_tier, status, per-asset balances) reflects a server-side
 * decision; this page has no authority of its own.
 */
export function AccountPage() {
  const { logout } = useAuth()
  const meQuery = useQuery({ queryKey: ['me'], queryFn: getMe })
  const walletsQuery = useQuery({ queryKey: ['wallets'], queryFn: listMyWallets })

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="My account"
        actions={
          <Button variant="secondary" onClick={logout}>
            Log out
          </Button>
        }
      />

      <Card title="Profile">
        {meQuery.isLoading && <LoadingSpinner label="Loading profile..." />}
        {meQuery.error && <ErrorState error={meQuery.error} onRetry={() => void meQuery.refetch()} />}
        {meQuery.data && (
          <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-slate-500">Email</dt>
              <dd className="mt-1 text-slate-900">{meQuery.data.email}</dd>
            </div>
            <div>
              <dt className="text-slate-500">Status</dt>
              <dd className="mt-1">
                <Badge tone={meQuery.data.status === 'active' ? 'success' : 'warning'}>{meQuery.data.status}</Badge>
              </dd>
            </div>
            <div>
              <dt className="text-slate-500">KYC tier</dt>
              <dd className="mt-1 text-slate-900">{meQuery.data.kyc_tier}</dd>
            </div>
          </dl>
        )}
      </Card>

      {meQuery.data?.status === 'pending_verification' && <EmailVerificationCard />}

      <Card
        title="Wallets"
        actions={
          <div className="flex gap-2">
            <Link to="/account/deposit">
              <Button variant="secondary">Deposit</Button>
            </Link>
            <Link to="/account/withdraw">
              <Button variant="secondary">Withdraw</Button>
            </Link>
          </div>
        }
      >
        {walletsQuery.isLoading && <LoadingSpinner label="Loading wallets..." />}
        {walletsQuery.error && <ErrorState error={walletsQuery.error} onRetry={() => void walletsQuery.refetch()} />}
        {walletsQuery.data && walletsQuery.data.length === 0 && (
          <EmptyState message="No wallets yet - make a deposit to create one." />
        )}
        {walletsQuery.data && walletsQuery.data.length > 0 && (
          <div className="flex flex-col divide-y divide-border">
            {walletsQuery.data.map((w) => (
              <div key={w.wallet_id} className="grid grid-cols-2 gap-2 py-3 text-sm sm:grid-cols-6">
                <div className="font-medium text-slate-900">{w.asset_code}</div>
                <div>
                  <dt className="text-xs text-slate-500">Available</dt>
                  <dd className="font-medium">{formatMoney(w.available_balance, w.asset_code)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-slate-500">Cash</dt>
                  <dd>{formatMoney(w.cash_balance, w.asset_code)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-slate-500">Held</dt>
                  <dd>{formatMoney(w.held_for_withdrawal, w.asset_code)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-slate-500">Locked</dt>
                  <dd>{formatMoney(w.locked_balance, w.asset_code)}</dd>
                </div>
                <div>
                  <dt className="text-xs text-slate-500">Bonus</dt>
                  <dd>{formatMoney(w.bonus_balance, w.asset_code)}</dd>
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      {meQuery.data && <ComplianceStatusCard kycTier={meQuery.data.kyc_tier} />}

      <Link to="/account/bets" className="text-sm text-brand-700 hover:underline">
        View my bet history &rarr;
      </Link>
    </div>
  )
}
