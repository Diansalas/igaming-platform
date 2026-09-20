import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import type { AuditEntry, ListAuditLogParams } from '../../api/audit'
import type { PagedResponse } from '../../api/types'
import { Badge } from '../../components/Badge'
import { PageHeader } from '../../components/PageHeader'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'

const OUTCOME_OPTIONS = [
  { value: 'success', label: 'Success' },
  { value: 'failure', label: 'Failure' },
  { value: 'denied', label: 'Denied' },
]

const ACTOR_TYPE_OPTIONS = [
  { value: 'staff', label: 'Staff' },
  { value: 'player', label: 'Player' },
  { value: 'system', label: 'System' },
  { value: 'service', label: 'Service' },
]

function outcomeTone(outcome: string) {
  if (outcome === 'success') return 'success' as const
  if (outcome === 'denied') return 'warning' as const
  return 'danger' as const
}

export interface AuditLogViewerProps {
  title: string
  description?: string
  /** The only thing that differs between the tenant-scoped and platform-scoped audit pages. */
  fetcher: (params: ListAuditLogParams) => Promise<PagedResponse<AuditEntry>>
  queryKeyPrefix: string
}

/**
 * ONE reusable audit viewer, used for both the tenant-scoped and the
 * platform-scoped audit log (Stage 5 directive's own reusability
 * requirement) - only the `fetcher`/`queryKeyPrefix` differ per caller.
 */
export function AuditLogViewer({ title, description, fetcher, queryKeyPrefix }: AuditLogViewerProps) {
  const [actorType, setActorType] = useState('')
  const [action, setAction] = useState('')
  const [outcome, setOutcome] = useState('')
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: [queryKeyPrefix, { actorType, action, outcome, limit, offset }],
    queryFn: () =>
      fetcher({
        actor_type: actorType || undefined,
        action: action || undefined,
        outcome: outcome || undefined,
        limit,
        offset,
      }),
  })

  const columns: Column<AuditEntry>[] = [
    { key: 'created_at', header: 'Time', render: (e) => e.created_at },
    { key: 'action', header: 'Action', render: (e) => e.action },
    { key: 'actor', header: 'Actor', render: (e) => `${e.actor_type}${e.actor_id ? ` (${e.actor_id})` : ''}` },
    { key: 'target', header: 'Target', render: (e) => (e.target_type ? `${e.target_type}/${e.target_id}` : '—') },
    { key: 'outcome', header: 'Outcome', render: (e) => <Badge tone={outcomeTone(e.outcome)}>{e.outcome}</Badge> },
  ]

  return (
    <div>
      <PageHeader title={title} description={description} />
      <div className="mb-4 flex flex-wrap gap-2">
        <Select
          placeholder="All actor types"
          options={ACTOR_TYPE_OPTIONS}
          value={actorType}
          onChange={(e) => {
            setActorType(e.target.value)
            resetToFirstPage()
          }}
        />
        <input
          type="text"
          placeholder="Action (exact match)"
          value={action}
          onChange={(e) => {
            setAction(e.target.value)
            resetToFirstPage()
          }}
          className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
        />
        <Select
          placeholder="All outcomes"
          options={OUTCOME_OPTIONS}
          value={outcome}
          onChange={(e) => {
            setOutcome(e.target.value)
            resetToFirstPage()
          }}
        />
      </div>
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(e) => e.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No audit entries match this filter."
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
    </div>
  )
}
