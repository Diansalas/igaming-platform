import type { LaunchTransition } from '../../api/launch'
import { Badge } from '../../components/Badge'
import { actorLabel } from './LaunchBits'
import { LEGACY_BASELINE_LABEL, subjectStatusLabel } from './labels'

const KIND_LABEL: Record<string, string> = {
  governed: 'Governed decision',
  legacy_baseline: LEGACY_BASELINE_LABEL,
  owner_provisioned: 'Provisioned directly by the database owner (no launch decision)',
}

/** Append-only status history from launch-status-history, newest first. */
export function HistoryTimeline({ items }: { items: LaunchTransition[] }) {
  if (items.length === 0) return <p className="text-sm text-slate-500">No status history recorded.</p>
  return (
    <ol className="flex flex-col gap-2 border-l-2 border-border pl-4" aria-label="Status history">
      {items.map((t) => (
        <li key={t.id} className="text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={t.kind === 'governed' ? 'info' : 'neutral'}>{KIND_LABEL[t.kind] ?? t.kind}</Badge>
            <span className="font-medium text-slate-800">
              {t.from_status ? `${subjectStatusLabel(t.from_status)} to ` : 'Recorded as '}
              {subjectStatusLabel(t.to_status)}
            </span>
          </div>
          <p className="text-xs text-slate-500">
            {new Date(t.created_at).toLocaleString()} - executed by {actorLabel(t.executed_by, t.actor_type)}
            {t.approver_ids.length > 0 && `; approvers: ${t.approver_ids.map((a) => a.slice(0, 8)).join(', ')}`}
            {t.request_id && <span className="font-mono"> - request {t.request_id.slice(0, 8)}</span>}
          </p>
        </li>
      ))}
    </ol>
  )
}
