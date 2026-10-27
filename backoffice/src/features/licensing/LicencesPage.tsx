import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { listJurisdictions, listLicences, type LicenceDetail } from '../../api/licensing'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { Button } from '../../components/Button'
import { SelectField, TextInput } from '../../components/FormField'
import { Table, type Column } from '../../components/Table'
import { FileChangeDialog } from './FileChangeDialog'
import { LicenceStatusBadge, LicensingErrorAlert } from './LicensingBits'
import { dateText, licenseeLabel } from './labels'

export function LicencesPage() {
  const { claims } = useAuth()
  const perms = getNavPermissions(claims?.role)
  const [q, setQ] = useState('')
  const [status, setStatus] = useState('')
  const [dialog, setDialog] = useState(false)
  const list = useQuery({ queryKey: ['licensing', 'licences'], queryFn: listLicences, retry: false })
  const jur = useQuery({ queryKey: ['licensing', 'jurisdictions'], queryFn: listJurisdictions, retry: false })
  const code = new Map((jur.data ?? []).map((j) => [j.id, j.code]))
  const needle = q.trim().toLowerCase()
  const rows = (list.data ?? []).filter(
    (l) => (status === '' || l.status === status) && (needle === '' || `${l.licence_number} ${code.get(l.jurisdiction_id) ?? ''}`.toLowerCase().includes(needle)),
  )
  const columns: Column<LicenceDetail>[] = [
    { key: 'number', header: 'Licence number (as supplied)', render: (l) => <Link className="font-medium text-brand-700 hover:underline" to={`/licensing/licences/${encodeURIComponent(l.id)}`}>{l.licence_number}</Link> },
    { key: 'jur', header: 'Jurisdiction', render: (l) => code.get(l.jurisdiction_id) ?? 'Unknown' },
    { key: 'licensee', header: 'Licensee', render: (l) => licenseeLabel(l.licensee) },
    { key: 'status', header: 'Status', render: (l) => <LicenceStatusBadge status={l.status} /> },
    { key: 'expires', header: 'Expires', render: (l) => dateText(l.expires_at) },
    { key: 'review', header: 'Review due', render: (l) => dateText(l.review_due_at) },
    { key: 'tenants', header: 'Tenants bound', render: (l) => l.tenant_ids.length },
  ]
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end gap-3">
        <TextInput label="Filter licences" value={q} onChange={(e) => setQ(e.target.value)} autoComplete="off" />
        <SelectField
          label="Status"
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          options={[{ value: '', label: 'Any status' }, { value: 'active', label: 'Active' }, { value: 'suspended', label: 'Suspended' }, { value: 'expired', label: 'Expired' }]}
        />
        {perms.licensingRequest && <Button onClick={() => setDialog(true)}>File a licence change</Button>}
      </div>
      <p className="text-xs text-slate-600">Status here is what staff recorded. It is not a statement from any regulator and is not verified by the platform.</p>
      {list.error ? <LicensingErrorAlert error={list.error} /> : null}
      <Table columns={columns} rows={rows} getRowKey={(l) => l.id} isLoading={list.isLoading} emptyMessage="No licences are recorded." />
      {dialog && <FileChangeDialog onClose={() => setDialog(false)} />}
    </div>
  )
}
