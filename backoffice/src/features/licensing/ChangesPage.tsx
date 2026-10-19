import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { listLicensingChanges, type ChangeStatus, type LicensingChange } from '../../api/licensing'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { SelectField } from '../../components/FormField'
import { Table, type Column } from '../../components/Table'
import { FileChangeDialog } from './FileChangeDialog'
import { ApprovalRules, ChangeStatusBadge, Disclaimer, LicensingErrorAlert, short } from './LicensingBits'
import { KIND_FILTER_OPTIONS, kindLabel, when } from './labels'

const STATUS_OPTIONS: { value: ChangeStatus | ''; label: string }[] = [
  { value: '', label: 'Any status' },
  { value: 'requested', label: 'Pending approval' },
  { value: 'approved', label: 'Approved, not yet executed' },
  { value: 'executed', label: 'Executed' },
  { value: 'rejected', label: 'Rejected' },
  { value: 'cancelled', label: 'Cancelled' },
  { value: 'expired', label: 'Expired' },
]
/** The list route takes ?limit (max 200) and no offset: the newest 200 matching requests are shown. */
const LIST_LIMIT = 200

export function ChangesPage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const [status, setStatus] = useState<ChangeStatus | ''>('requested')
  const [kind, setKind] = useState('')
  const [dialog, setDialog] = useState(false)
  const list = useQuery({
    queryKey: ['licensing', 'changes', status, kind],
    queryFn: () => listLicensingChanges({ status: status || undefined, kind: kind || undefined, limit: LIST_LIMIT }),
    retry: false,
  })
  const columns: Column<LicensingChange>[] = [
    { key: 'filed', header: 'Filed', render: (c) => <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/changes/${c.id}`}>{when(c.created_at)}</Link> },
    { key: 'kind', header: 'Kind', render: (c) => kindLabel(c.kind) },
    { key: 'status', header: 'Status', render: (c) => <ChangeStatusBadge status={c.status} /> },
    { key: 'class', header: 'Class', render: (c) => (c.widening ? 'Widening' : 'Narrowing or neutral') },
    { key: 'appr', header: 'Approvals', render: (c) => `${c.approvals.length} of ${c.required_approvals}` },
    { key: 'by', header: 'Requested by', render: (c) => (c.requested_by ? `Platform staff (${short(c.requested_by)})` : 'Not shown') },
    { key: 'exp', header: 'Request expires', render: (c) => when(c.expires_at) },
  ]
  return (
    <div className="flex flex-col gap-3">
      <Disclaimer />
      <ApprovalRules />
      <div className="flex flex-wrap items-end gap-3">
        <SelectField label="Status" value={status} onChange={(e) => setStatus(e.target.value as ChangeStatus | '')} options={STATUS_OPTIONS} />
        <SelectField label="Kind" value={kind} onChange={(e) => setKind(e.target.value)} options={[{ value: '', label: 'Any kind' }, ...KIND_FILTER_OPTIONS]} />
        <Button variant="secondary" onClick={() => void list.refetch()} isLoading={list.isFetching}>Refresh</Button>
        {perms.licensingRequest && <Button onClick={() => setDialog(true)}>File a change</Button>}
      </div>
      {list.error ? <LicensingErrorAlert error={list.error} /> : null}
      <Table columns={columns} rows={list.data?.items ?? []} getRowKey={(c) => c.id} isLoading={list.isLoading} emptyMessage="No licensing changes match." />
      {(list.data?.items.length ?? 0) >= LIST_LIMIT && <p className="text-xs text-slate-600">Showing the newest {LIST_LIMIT}. Narrow the filters to see older requests.</p>}
      {dialog && <FileChangeDialog onClose={() => setDialog(false)} />}
    </div>
  )
}
