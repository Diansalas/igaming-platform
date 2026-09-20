import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { listKycCases, type KycCase } from '../../api/kyc'
import { Badge } from '../../components/Badge'
import { Card } from '../../components/Card'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { KycCaseDetail } from './KycCaseDetail'
import { kycStatusTone } from './status'

/** Player-detail sub-section: this player's own KYC/verification cases. */
export function PlayerKycSummary({ playerAccountId }: { playerAccountId: string }) {
  const [selected, setSelected] = useState<KycCase | null>(null)
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['kyc-cases', 'player', playerAccountId],
    queryFn: () => listKycCases({ player_account_id: playerAccountId, limit: 20, offset: 0 }),
  })

  return (
    <Card title="KYC">
      {isLoading ? (
        <LoadingSpinner />
      ) : error ? (
        <ErrorState error={error} onRetry={() => void refetch()} />
      ) : !data || data.items.length === 0 ? (
        <p className="text-sm text-slate-500">No verification cases on file.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {data.items.map((c) => (
            <li key={c.id}>
              <button
                type="button"
                onClick={() => setSelected(c)}
                className="flex w-full items-center justify-between rounded-md border border-border px-3 py-2 text-left text-sm hover:bg-surface-alt"
              >
                <span className="font-mono text-xs text-slate-500">{c.provider_id}</span>
                <Badge tone={kycStatusTone(c.status)}>{c.status}</Badge>
              </button>
            </li>
          ))}
        </ul>
      )}
      {selected && <KycCaseDetail kycCase={selected} onClose={() => setSelected(null)} />}
    </Card>
  )
}
