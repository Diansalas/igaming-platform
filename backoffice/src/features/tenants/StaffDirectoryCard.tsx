import { useQuery } from '@tanstack/react-query'
import { listTenantStaff, type StaffDirectoryEntry, type StaffStatus } from '../../api/tenants'
import { Badge, type BadgeTone } from '../../components/Badge'
import { ApiError } from '../../api/types'
import { Button } from '../../components/Button'
import { Card } from '../../components/Card'
import { Table, type Column } from '../../components/Table'
import { describeStaffListError } from './staffErrors'

const STATUS_LABEL: Record<StaffStatus, string> = {
  invited: 'Invited',
  active: 'Active',
  suspended: 'Suspended',
  offboarded: 'Offboarded',
}

const STATUS_TONE: Record<StaffStatus, BadgeTone> = { invited: 'info', active: 'success', suspended: 'warning', offboarded: 'neutral' }

function fmt(ts: string | null): string {
  if (!ts) return '-'
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? '-' : d.toISOString().replace('T', ' ').slice(0, 16) + ' UTC'
}

/**
 * GET /v1/admin/tenants/{tenantID}/staff - staff:read. READ-ONLY directory with the lifecycle fields the API returns
 * (status, MFA, Person link, last login, status change). There are no suspend / offboard / role-change / resend actions
 * here: those are ADR 0119 S8 (a later slice). Every value is rendered as text; nothing from the server is interpreted
 * as markup.
 */
export function StaffDirectoryCard({ tenantId }: { tenantId: string }) {
  const query = useQuery({
    queryKey: ['tenant-staff', tenantId],
    queryFn: () => listTenantStaff(tenantId),
    enabled: !!tenantId,
  })

  const columns: Column<StaffDirectoryEntry>[] = [
    {
      key: 'email',
      header: 'Email',
      render: (s) => (
        <span>
          <span className="font-medium text-slate-900"><bdi>{s.email}</bdi></span>
          {s.display_name && <span className="ml-2 text-slate-500"><bdi>{s.display_name}</bdi></span>}
        </span>
      ),
    },
    { key: 'role', header: 'Role', render: (s) => s.role },
    {
      key: 'status',
      header: 'Status',
      render: (s) => <Badge tone={STATUS_TONE[s.status] ?? 'neutral'}>{STATUS_LABEL[s.status] ?? s.status}</Badge>,
    },
    {
      key: 'mfa',
      header: 'Authenticator',
      render: (s) => (s.mfa.hard_locked ? 'Locked' : s.mfa.enrolled ? 'Enrolled' : 'Not enrolled'),
    },
    { key: 'person', header: 'Person', render: (s) => (s.person_linked ? 'Linked' : 'Not linked') },
    { key: 'last_login', header: 'Last sign-in', render: (s) => fmt(s.last_login_at) },
    { key: 'changed', header: 'Status changed', render: (s) => fmt(s.status_changed_at) },
  ]

  return (
    <Card title="Staff">
      {query.error ? (
        // Fixed, code-keyed wording: the server's message text is never rendered.
        <div role="alert" className="flex flex-col items-center gap-3 rounded-lg border border-red-200 bg-red-50 p-6 text-center">
          <p className="text-sm font-medium text-red-800">{describeStaffListError(query.error)}</p>
          {query.error instanceof ApiError && query.error.requestId && (
            <p className="text-xs text-red-500">Request ID: {query.error.requestId}</p>
          )}
          <Button variant="secondary" onClick={() => void query.refetch()}>
            Try again
          </Button>
        </div>
      ) : (
        <Table
          columns={columns}
          rows={query.data?.staff ?? []}
          getRowKey={(s) => s.id}
          isLoading={query.isLoading}
          emptyMessage="This tenant has no staff yet."
        />
      )}
      {query.data?.truncated && (
        <p className="mt-2 text-xs text-amber-700">Showing the first 500 staff members; the list is truncated.</p>
      )}
    </Card>
  )
}
