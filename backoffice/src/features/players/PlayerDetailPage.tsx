import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { getPlayer, reinstatePlayer, suspendPlayer } from '../../api/players'
import { ApiError } from '../../api/types'
import { Badge } from '../../components/Badge'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorState } from '../../components/ErrorState'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { PageHeader } from '../../components/PageHeader'
import { PlayerBonusSummary } from '../bonus/PlayerBonusSummary'
import { PlayerKycSummary } from '../kyc/PlayerKycSummary'
import { PlayerRgSummary } from '../rg/PlayerRgSummary'
import { playerStatusTone } from './status'

type DialogState = 'suspend' | 'reinstate' | null

export function PlayerDetailPage() {
  const { id = '' } = useParams()
  const queryClient = useQueryClient()
  const [dialog, setDialog] = useState<DialogState>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const playerQuery = useQuery({
    queryKey: ['player', id],
    queryFn: () => getPlayer(id),
    enabled: !!id,
  })

  const suspendMutation = useMutation({
    mutationFn: (reason: string) => suspendPlayer(id, reason),
    onSuccess: async () => {
      setDialog(null)
      setActionError(null)
      await queryClient.invalidateQueries({ queryKey: ['player', id] })
    },
    onError: (err) => setActionError(err instanceof ApiError ? err.message : 'Failed to suspend player.'),
  })

  const reinstateMutation = useMutation({
    mutationFn: (reasonCode: string) => reinstatePlayer(id, reasonCode),
    onSuccess: async () => {
      setDialog(null)
      setActionError(null)
      await queryClient.invalidateQueries({ queryKey: ['player', id] })
    },
    onError: (err) => setActionError(err instanceof ApiError ? err.message : 'Failed to reinstate player.'),
  })

  if (playerQuery.isLoading) return <LoadingSpinner label="Loading player..." />
  if (playerQuery.error) return <ErrorState error={playerQuery.error} onRetry={() => void playerQuery.refetch()} />
  const player = playerQuery.data
  if (!player) return null

  const canSuspend = player.status === 'active' || player.status === 'pending_verification'
  const canReinstate = player.status === 'suspended'

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={player.email}
        description={`Player ID: ${player.id}`}
        actions={
          <>
            {canSuspend && (
              <Button
                variant="danger"
                onClick={() => {
                  setActionError(null)
                  setDialog('suspend')
                }}
              >
                Suspend
              </Button>
            )}
            {canReinstate && (
              <Button
                variant="primary"
                onClick={() => {
                  setActionError(null)
                  setDialog('reinstate')
                }}
              >
                Reinstate
              </Button>
            )}
          </>
        }
      />

      <Card title="Account">
        <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500">Status</dt>
            <dd className="mt-1">
              <Badge tone={playerStatusTone(player.status)}>{player.status}</Badge>
            </dd>
          </div>
          <div>
            <dt className="text-slate-500">KYC tier</dt>
            <dd className="mt-1 font-medium text-slate-900">{player.kyc_tier}</dd>
          </div>
          <div>
            <dt className="text-slate-500">Brand ID</dt>
            <dd className="mt-1 break-all font-mono text-xs text-slate-700">{player.brand_id}</dd>
          </div>
        </dl>
      </Card>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <PlayerKycSummary playerAccountId={player.id} />
        <PlayerRgSummary playerAccountId={player.id} />
        <PlayerBonusSummary playerAccountId={player.id} />
      </div>

      {/* Recent audit activity for this player is deliberately NOT shown here:
          GET /v1/admin/audit-log only supports actor_type/action/outcome
          filters, not a target-player filter (internal/httpserver/admin_routes.go's
          queryAuditLog) - inventing a client-side filter over an unbounded,
          unpaginated scan would silently return an incomplete picture, so
          this sub-section is intentionally left out rather than faked.
          See the Stage 5 completion report. */}

      <Link to="/players" className="text-sm text-brand-700 hover:underline">
        &larr; Back to players
      </Link>

      {dialog === 'suspend' && (
        <ConfirmDialog
          title="Suspend player"
          description="This immediately suspends the player's account. A reason is required and this action is written to the audit log."
          variant="danger"
          confirmLabel="Suspend"
          requireReason
          reasonLabel="Reason"
          isSubmitting={suspendMutation.isPending}
          errorMessage={actionError}
          onConfirm={(reason) => suspendMutation.mutate(reason)}
          onCancel={() => {
            setDialog(null)
            setActionError(null)
          }}
        />
      )}
      {dialog === 'reinstate' && (
        <ConfirmDialog
          title="Reinstate player"
          description="This lifts the suspension and restores the player's active status. A reason code is required and this action is written to the audit log."
          confirmLabel="Reinstate"
          requireReason
          reasonLabel="Reason code"
          isSubmitting={reinstateMutation.isPending}
          errorMessage={actionError}
          onConfirm={(reason) => reinstateMutation.mutate(reason)}
          onCancel={() => {
            setDialog(null)
            setActionError(null)
          }}
        />
      )}
    </div>
  )
}
