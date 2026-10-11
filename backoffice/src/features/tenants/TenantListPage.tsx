import { useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { listTenants } from '../../api/tenants'
import { PageHeader } from '../../components/PageHeader'
import { SearchInput } from '../../components/SearchInput'
import { Select } from '../../components/Select'
import { Table, type Column } from '../../components/Table'
import { usePagination } from '../../components/usePagination'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { useState } from 'react'
import type { Tenant } from '../../api/tenants'
import { useAuth } from '../../auth/AuthContext'
import { getNavPermissions } from '../../auth/permissions'
import { CreateTenantForm } from './CreateTenantForm'

import { SUBJECT_STATUS_FILTER_OPTIONS } from '../launch/labels'
import { LaunchStatusBadge } from '../launch/LaunchBits'

const STATUS_OPTIONS = SUBJECT_STATUS_FILTER_OPTIONS

export function TenantListPage() {
  const navigate = useNavigate()
  const { claims } = useAuth()
  const canCreateTenant = getNavPermissions(claims?.role).tenantWrite
  const [q, setQ] = useState('')
  const [status, setStatus] = useState('')
  const debouncedQ = useDebouncedValue(q, 300)
  const { limit, offset, setOffset, resetToFirstPage } = usePagination()

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['tenants', { q: debouncedQ, status, limit, offset }],
    queryFn: () => listTenants({ q: debouncedQ || undefined, status: status || undefined, limit, offset }),
  })

  const columns: Column<Tenant>[] = [
    { key: 'name', header: 'Name', render: (t) => <span className="font-medium text-slate-900">{t.name}</span> },
    { key: 'slug', header: 'Slug', render: (t) => t.slug },
    { key: 'licensing_model', header: 'Licensing model', render: (t) => t.licensing_model },
    { key: 'status', header: 'Status', render: (t) => <LaunchStatusBadge status={t.status} /> },
  ]

  return (
    <div>
      <PageHeader title="Tenants" description="Platform-wide tenant list." />
      <div className="mb-4 flex flex-wrap gap-2">
        <SearchInput
          placeholder="Search by name or slug"
          value={q}
          onChange={(e) => {
            setQ(e.target.value)
            resetToFirstPage()
          }}
        />
        <Select
          placeholder="All statuses"
          options={STATUS_OPTIONS}
          value={status}
          onChange={(e) => {
            setStatus(e.target.value)
            resetToFirstPage()
          }}
        />
      </div>
      <Table
        columns={columns}
        rows={data?.items ?? []}
        getRowKey={(t) => t.id}
        isLoading={isLoading}
        error={error}
        onRetry={() => void refetch()}
        emptyMessage="No tenants match this filter."
        onRowClick={(t) => navigate(`/tenants/${t.id}`)}
        pagination={data ? { limit: data.limit, offset: data.offset, total: data.total, onPageChange: setOffset } : undefined}
      />
      {canCreateTenant && (
        <div className="mt-6">
          <CreateTenantForm />
        </div>
      )}
    </div>
  )
}
